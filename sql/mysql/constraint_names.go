// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package mysql

import (
	"fmt"
	"regexp"
	"strings"

	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"
)

var _ sqlx.TableChangesAnnotator = (*diff)(nil)

// ConstraintNameStrategy controls how constraint names are matched during diffing.
type ConstraintNameStrategy string

const (
	// ConstraintNamesStrict requires constraint names to match exactly.
	ConstraintNamesStrict ConstraintNameStrategy = "strict"
	// ConstraintNamesIgnoreVitess ignores deterministic suffixes added by Vitess Online DDL.
	ConstraintNamesIgnoreVitess ConstraintNameStrategy = "ignore_vitess"
	// ConstraintNamesIgnoreAll matches constraints by definition regardless of their names.
	ConstraintNamesIgnoreAll ConstraintNameStrategy = "ignore_all"
)

// DiffOptions defines MySQL-specific schema diff options.
type DiffOptions struct {
	ConstraintNames struct {
		Strategy ConstraintNameStrategy `spec:"strategy"`
	} `spec:"constraint_names"`
	extra any
}

// DiffConstraintNames configures how MySQL constraint names are matched.
func DiffConstraintNames(strategy ConstraintNameStrategy) schema.DiffOption {
	return func(opts *schema.DiffOptions) {
		switch extra := opts.Extra.(type) {
		case DiffOptions:
			extra.ConstraintNames.Strategy = strategy
			opts.Extra = extra
		case *DiffOptions:
			if extra == nil {
				extra = &DiffOptions{}
			}
			extra.ConstraintNames.Strategy = strategy
			opts.Extra = extra
		default:
			wrapped := &DiffOptions{extra: opts.Extra}
			wrapped.ConstraintNames.Strategy = strategy
			opts.Extra = wrapped
		}
	}
}

const (
	// MySQL limits constraint identifiers to 64 bytes.
	mysqlMaxConstraintNameLen = 64
	// Vitess appends "_" and a 25-character base-36 hash to constraint names.
	vitessConstraintHashLen   = 25
	vitessConstraintSuffixLen = 1 + vitessConstraintHashLen
)

var vitessConstraintName = regexp.MustCompile(fmt.Sprintf(`^(.*?)(_(?:[0-9a-z]{%d}))?$`, vitessConstraintHashLen))

// AnnotateTableChanges implements sqlx.TableChangesAnnotator.
func (d *diff) AnnotateTableChanges(table *schema.Table, changes []schema.Change, opts *schema.DiffOptions) ([]schema.Change, error) {
	extra, err := mysqlDiffOptions(opts)
	if err != nil {
		return nil, err
	}
	strategy := extra.ConstraintNames.Strategy
	if strategy == "" {
		strategy = ConstraintNamesStrict
	}
	switch strategy {
	case ConstraintNamesStrict:
		return changes, nil
	case ConstraintNamesIgnoreVitess, ConstraintNamesIgnoreAll:
		modify := &schema.ModifyTable{T: table, Changes: changes}
		d.filterConstraintNameChanges(modify, strategy)
		return modify.Changes, nil
	default:
		return nil, fmt.Errorf("mysql: unknown constraint-name strategy %q", strategy)
	}
}

func mysqlDiffOptions(opts *schema.DiffOptions) (*DiffOptions, error) {
	return decodeDiffOptions(opts.Extra)
}

func decodeDiffOptions(value any) (*DiffOptions, error) {
	var extra DiffOptions
	switch v := value.(type) {
	case nil:
	case DiffOptions:
		base, err := decodeDiffOptions(v.extra)
		if err != nil {
			return nil, err
		}
		extra = *base
		if v.ConstraintNames.Strategy != "" {
			extra.ConstraintNames.Strategy = v.ConstraintNames.Strategy
		}
	case *DiffOptions:
		if v != nil {
			return decodeDiffOptions(*v)
		}
	case schemahcl.DefaultExtension:
		if err := v.Extra.As(&extra); err != nil {
			return nil, err
		}
	case *schemahcl.DefaultExtension:
		if v != nil {
			if err := v.Extra.As(&extra); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("mysql: unexpected DiffOptions.Extra type %T", value)
	}
	return &extra, nil
}

type constraintChangePair[T any] struct {
	drop, add int
	from, to  T
}

func (d *diff) filterConstraintNameChanges(modify *schema.ModifyTable, strategy ConstraintNameStrategy) {
	remove := make(map[int]bool)
	foreignKeys := pairConstraintChanges(
		modify.Changes,
		func(change schema.Change) (*schema.ForeignKey, bool) {
			drop, ok := change.(*schema.DropForeignKey)
			if !ok {
				return nil, false
			}
			return drop.F, true
		},
		func(change schema.Change) (*schema.ForeignKey, bool) {
			add, ok := change.(*schema.AddForeignKey)
			if !ok {
				return nil, false
			}
			return add.F, true
		},
		func(from, to *schema.ForeignKey) bool {
			return constraintNamesMatch(strategy, modify.T.Name, from.Symbol, to.Symbol) && d.foreignKeysEqual(from, to)
		},
	)
	for _, pair := range foreignKeys {
		drop := findIndexChange(modify.Changes, pair.from.Symbol, true, remove)
		add := findIndexChange(modify.Changes, pair.to.Symbol, false, remove)
		switch {
		case drop == -1 && add == -1:
			remove[pair.drop], remove[pair.add] = true, true
		case drop != -1 && add != -1:
			dropIndex := modify.Changes[drop].(*schema.DropIndex).I
			addIndex := modify.Changes[add].(*schema.AddIndex).I
			if sqlx.IndexEqual(d, dropIndex, addIndex) {
				remove[pair.drop], remove[pair.add] = true, true
				remove[drop], remove[add] = true, true
			}
		case (drop == -1) != (add == -1):
			// A one-sided AddIndex is a genuine declared change, hence it is kept
			// along with the foreign-key pair. A one-sided DropIndex is redundant
			// only if no other index on the desired table covers the foreign-key
			// columns as a prefix, i.e., applying the desired state from scratch
			// would recreate an equivalent implicit index.
			if add == -1 && !indexCoversForeignKey(modify.T, pair.to) {
				remove[pair.drop], remove[pair.add] = true, true
				remove[drop] = true
			}
		}
	}
	checks := pairConstraintChanges(
		modify.Changes,
		func(change schema.Change) (*schema.Check, bool) {
			drop, ok := change.(*schema.DropCheck)
			if !ok {
				return nil, false
			}
			return drop.C, true
		},
		func(change schema.Change) (*schema.Check, bool) {
			add, ok := change.(*schema.AddCheck)
			if !ok {
				return nil, false
			}
			return add.C, true
		},
		func(from, to *schema.Check) bool {
			return constraintNamesMatch(strategy, modify.T.Name, from.Name, to.Name) && checksEqual(from, to)
		},
	)
	for _, pair := range checks {
		remove[pair.drop], remove[pair.add] = true, true
	}
	filtered := modify.Changes[:0]
	for i, change := range modify.Changes {
		if !remove[i] {
			filtered = append(filtered, change)
		}
	}
	modify.Changes = filtered
}

func pairConstraintChanges[T any](
	changes []schema.Change,
	dropConstraint, addConstraint func(schema.Change) (T, bool),
	equal func(T, T) bool,
) []constraintChangePair[T] {
	var (
		pairs   []constraintChangePair[T]
		usedAdd = make(map[int]bool)
	)
	for i, change := range changes {
		from, ok := dropConstraint(change)
		if !ok {
			continue
		}
		for j, candidate := range changes {
			to, ok := addConstraint(candidate)
			if !ok || usedAdd[j] || !equal(from, to) {
				continue
			}
			usedAdd[j] = true
			pairs = append(pairs, constraintChangePair[T]{drop: i, add: j, from: from, to: to})
			break
		}
	}
	return pairs
}

func constraintNamesMatch(strategy ConstraintNameStrategy, table, from, to string) bool {
	switch strategy {
	case ConstraintNamesIgnoreAll:
		return true
	case ConstraintNamesIgnoreVitess:
		return vitessConstraintNamesMatch(table, from, to)
	default:
		return from == to
	}
}

func vitessConstraintNamesMatch(table, from, to string) bool {
	if from == to {
		return true
	}
	fromName := parseVitessConstraintName(table, from)
	toName := parseVitessConstraintName(table, to)
	if !fromName.generated() && !toName.generated() {
		return false
	}
	if fromName.base == toName.base {
		return true
	}
	// Vitess truncates the original name to leave room for "_" and its
	// 25-character hash within MySQL's 64-byte identifier limit. Because the
	// discarded characters cannot be recovered, callers must use strict mode
	// for a rename that differs only after this boundary.
	const maxPrefix = mysqlMaxConstraintNameLen - vitessConstraintSuffixLen
	return truncateConstraintName(fromName.base, maxPrefix) == truncateConstraintName(toName.base, maxPrefix) ||
		fromName.suffixed != toName.suffixed && truncateConstraintName(fromName.raw, maxPrefix) == truncateConstraintName(toName.raw, maxPrefix)
}

type parsedConstraintName struct {
	base, raw           string
	suffixed, automatic bool
}

func (n parsedConstraintName) generated() bool {
	return n.suffixed || n.automatic
}

func parseVitessConstraintName(table, name string) parsedConstraintName {
	parsed := parsedConstraintName{raw: name}
	if match := vitessConstraintName.FindStringSubmatch(name); len(match) > 0 && match[2] != "" {
		parsed.raw, parsed.suffixed = match[1], true
	}
	parsed.base = parsed.raw
	if prefix := table + "_chk_"; strings.HasPrefix(parsed.base, prefix) {
		parsed.base, parsed.automatic = parsed.base[len(table)+1:], true
	}
	if prefix := table + "_ibfk_"; strings.HasPrefix(parsed.base, prefix) {
		parsed.base, parsed.automatic = parsed.base[len(table)+1:], true
	}
	return parsed
}

func truncateConstraintName(name string, length int) string {
	if len(name) > length {
		return name[:length]
	}
	return name
}

func (d *diff) foreignKeysEqual(from, to *schema.ForeignKey) bool {
	if from.RefTable == nil || to.RefTable == nil || from.RefTable.Name != to.RefTable.Name || len(from.Columns) != len(to.Columns) || len(from.RefColumns) != len(to.RefColumns) {
		return false
	}
	for i := range from.Columns {
		if from.Columns[i].Name != to.Columns[i].Name || from.RefColumns[i].Name != to.RefColumns[i].Name {
			return false
		}
	}
	return !d.ReferenceChanged(from.OnUpdate, to.OnUpdate) &&
		!d.ReferenceChanged(from.OnDelete, to.OnDelete) &&
		!d.ForeignKeyAttrChanged(from.Attrs, to.Attrs)
}

func checksEqual(from, to *schema.Check) bool {
	return enforced(from.Attrs) == enforced(to.Attrs) &&
		(from.Expr == to.Expr || sqlx.MayWrap(from.Expr) == sqlx.MayWrap(to.Expr))
}

// indexCoversForeignKey reports if the table has an index that covers the
// foreign-key columns as a prefix, meaning MySQL would not create an implicit
// index to support the constraint.
func indexCoversForeignKey(t *schema.Table, fk *schema.ForeignKey) bool {
search:
	for _, idx := range t.Indexes {
		if len(idx.Parts) < len(fk.Columns) {
			continue
		}
		for i, c := range fk.Columns {
			if idx.Parts[i].C == nil || idx.Parts[i].C.Name != c.Name {
				continue search
			}
		}
		return true
	}
	return false
}

func findIndexChange(changes []schema.Change, name string, drop bool, removed map[int]bool) int {
	for i, change := range changes {
		if removed[i] {
			continue
		}
		if drop {
			if c, ok := change.(*schema.DropIndex); ok && c.I.Name == name {
				return i
			}
		} else if c, ok := change.(*schema.AddIndex); ok && c.I.Name == name {
			return i
		}
	}
	return -1
}
