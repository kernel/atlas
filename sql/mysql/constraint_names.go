// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package mysql

import (
	"fmt"
	"reflect"
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
}

// DiffConstraintNames configures how MySQL constraint names are matched.
func DiffConstraintNames(strategy ConstraintNameStrategy) schema.DiffOption {
	return func(opts *schema.DiffOptions) {
		extra := &DiffOptions{}
		extra.ConstraintNames.Strategy = strategy
		opts.Extra = extra
	}
}

const (
	mysqlMaxConstraintNameLen = 64
	vitessConstraintSuffixLen = 26
)

var vitessConstraintName = regexp.MustCompile(`^(.*?)(_([0-9a-z]{25}))?$`)

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
	var extra DiffOptions
	switch v := opts.Extra.(type) {
	case nil:
	case DiffOptions:
		extra = v
	case *DiffOptions:
		if v != nil {
			extra = *v
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
		return nil, fmt.Errorf("mysql: unexpected DiffOptions.Extra type %T", opts.Extra)
	}
	return &extra, nil
}

type foreignKeyPair struct {
	from, to *schema.ForeignKey
}

func (d *diff) filterConstraintNameChanges(modify *schema.ModifyTable, strategy ConstraintNameStrategy) {
	var (
		remove = make(map[int]bool)
		usedFK = make(map[int]bool)
		pairs  []foreignKeyPair
	)
	for i, change := range modify.Changes {
		drop, ok := change.(*schema.DropForeignKey)
		if !ok {
			continue
		}
		for j, candidate := range modify.Changes {
			add, ok := candidate.(*schema.AddForeignKey)
			if !ok || usedFK[j] || !constraintNamesMatch(strategy, modify.T.Name, drop.F.Symbol, add.F.Symbol) || !d.foreignKeysEqual(drop.F, add.F) {
				continue
			}
			remove[i], remove[j], usedFK[j] = true, true, true
			pairs = append(pairs, foreignKeyPair{from: drop.F, to: add.F})
			break
		}
	}
	for _, pair := range pairs {
		drop := findIndexChange(modify.Changes, pair.from.Symbol, true, remove)
		add := findIndexChange(modify.Changes, pair.to.Symbol, false, remove)
		if drop != -1 && add != -1 {
			dropIndex := modify.Changes[drop].(*schema.DropIndex).I
			addIndex := modify.Changes[add].(*schema.AddIndex).I
			if d.indexesEqual(dropIndex, addIndex) {
				remove[drop], remove[add] = true, true
			}
		}
	}
	usedCheck := make(map[int]bool)
	for i, change := range modify.Changes {
		drop, ok := change.(*schema.DropCheck)
		if !ok {
			continue
		}
		for j, candidate := range modify.Changes {
			add, ok := candidate.(*schema.AddCheck)
			if !ok || usedCheck[j] || !constraintNamesMatch(strategy, modify.T.Name, drop.C.Name, add.C.Name) || !checksEqual(drop.C, add.C) {
				continue
			}
			remove[i], remove[j], usedCheck[j] = true, true, true
			break
		}
	}
	filtered := modify.Changes[:0]
	for i, change := range modify.Changes {
		if !remove[i] {
			filtered = append(filtered, change)
		}
	}
	modify.Changes = filtered
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
	from, fromGenerated := parseVitessConstraintName(table, from)
	to, toGenerated := parseVitessConstraintName(table, to)
	if fromGenerated || toGenerated {
		const maxPrefix = mysqlMaxConstraintNameLen - vitessConstraintSuffixLen
		if len(from) > maxPrefix {
			from = from[:maxPrefix]
		}
		if len(to) > maxPrefix {
			to = to[:maxPrefix]
		}
	}
	return from == to
}

func vitessOriginalConstraintName(table, name string) string {
	name, _ = parseVitessConstraintName(table, name)
	return name
}

func parseVitessConstraintName(table, name string) (string, bool) {
	generated := false
	if match := vitessConstraintName.FindStringSubmatch(name); len(match) > 0 && match[2] != "" {
		name, generated = match[1], true
	}
	if prefix := table + "_chk_"; strings.HasPrefix(name, prefix) {
		return name[len(table)+1:], true
	}
	if prefix := table + "_ibfk_"; strings.HasPrefix(name, prefix) {
		return name[len(table)+1:], true
	}
	return name, generated
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

func (d *diff) indexesEqual(from, to *schema.Index) bool {
	if from.Unique != to.Unique || len(from.Parts) != len(to.Parts) || d.IndexAttrChanged(from.Attrs, to.Attrs) || sqlx.CommentDiff(from.Attrs, to.Attrs) != nil {
		return false
	}
	for i := range from.Parts {
		f, t := from.Parts[i], to.Parts[i]
		if f.Desc != t.Desc || d.IndexPartAttrChanged(from, to, i) {
			return false
		}
		switch {
		case f.C != nil && t.C != nil:
			if f.C.Name != t.C.Name {
				return false
			}
		case f.X != nil && t.X != nil:
			if !reflect.DeepEqual(f.X, t.X) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
