// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

//go:build !ent

package mysql

import (
	"context"
	"strings"
	"testing"

	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/internal/sqlx"
	"ariga.io/atlas/sql/schema"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

const vitessSuffix = "_1234567890123456789012345"

func TestDiff_ConstraintNames(t *testing.T) {
	differ := DefaultDiff.(*sqlx.Diff)
	_, ok := differ.DiffDriver.(sqlx.TableChangesAnnotator)
	require.True(t, ok)
	t.Run("strict", func(t *testing.T) {
		from := constraintTable(vitessSuffix, "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, true)
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized())
		require.NoError(t, err)
		require.IsType(t, &schema.DropCheck{}, changes[0])
		require.IsType(t, &schema.AddCheck{}, changes[1])
		require.IsType(t, &schema.AddColumn{}, changes[2])
		require.IsType(t, &schema.DropIndex{}, changes[3])
		require.IsType(t, &schema.AddIndex{}, changes[4])
		require.IsType(t, &schema.DropForeignKey{}, changes[5])
		require.IsType(t, &schema.AddForeignKey{}, changes[6])
	})
	t.Run("ignore vitess", func(t *testing.T) {
		from := constraintTable(vitessSuffix, "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, true)
		d := differ.DiffDriver.(*diff)
		require.True(t, constraintNamesMatch(ConstraintNamesIgnoreVitess, from.Name, from.ForeignKeys[0].Symbol, to.ForeignKeys[0].Symbol))
		require.True(t, d.foreignKeysEqual(from.ForeignKeys[0], to.ForeignKeys[0]))
		require.True(t, checksEqual(from.Attrs[0].(*schema.Check), to.Attrs[0].(*schema.Check)))
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		require.Equal(t, []schema.Change{&schema.AddColumn{C: to.Columns[2]}}, changes)
	})
	t.Run("ignore vitess auto-generated names with suffix", func(t *testing.T) {
		from := constraintTable("", "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, true)
		renameConstraints(from, "children_ibfk_1"+vitessSuffix, "children_chk_1"+vitessSuffix)
		renameConstraints(to, "children_ibfk_1", "children_chk_1")
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		require.Equal(t, []schema.Change{&schema.AddColumn{C: to.Columns[2]}}, changes)
	})
	t.Run("ignore vitess truncated prefix", func(t *testing.T) {
		const (
			fkName    = "children_parent_fk_name_longer_than_thirty_eight"
			checkName = "children_id_positive_name_longer_than_thirty_eight"
			maxPrefix = mysqlMaxConstraintNameLen - vitessConstraintSuffixLen
		)
		from := constraintTable("", "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, true)
		renameConstraints(from, fkName[:maxPrefix]+vitessSuffix, checkName[:maxPrefix]+vitessSuffix)
		renameConstraints(to, fkName, checkName)
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		require.Equal(t, []schema.Change{&schema.AddColumn{C: to.Columns[2]}}, changes)
	})
	t.Run("ignore vitess with asymmetric index changes", func(t *testing.T) {
		from := constraintTable(vitessSuffix, "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, false)
		to.Indexes = nil
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		require.Empty(t, changes)
	})
	t.Run("ignore all", func(t *testing.T) {
		from := constraintTable("_old", "`id` > 0", schema.Cascade, false)
		to := constraintTable("_new", "`id` > 0", schema.Cascade, true)
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreAll))
		require.NoError(t, err)
		require.Equal(t, []schema.Change{&schema.AddColumn{C: to.Columns[2]}}, changes)
	})
	t.Run("preserves cardinality", func(t *testing.T) {
		from := constraintTable(vitessSuffix, "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, false)
		const duplicate = "children_parent_fk_aaaaaaaaaaaaaaaaaaaaaaaaa"
		from.AddIndexes(schema.NewIndex(duplicate).AddParts(schema.NewIndexPart().SetColumn(from.Columns[1])))
		from.AddForeignKeys(schema.NewForeignKey(duplicate).
			SetTable(from).
			AddColumns(from.Columns[1]).
			SetRefTable(from.ForeignKeys[0].RefTable).
			AddRefColumns(from.ForeignKeys[0].RefColumns[0]).
			SetOnUpdate(schema.NoAction).
			SetOnDelete(schema.Cascade))
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		require.Len(t, changes, 2)
		require.Equal(t, duplicate, changes[0].(*schema.DropIndex).I.Name)
		require.Equal(t, duplicate, changes[1].(*schema.DropForeignKey).F.Symbol)
	})
	t.Run("non-vitess rename", func(t *testing.T) {
		from := constraintTable("_old", "`id` > 0", schema.Cascade, false)
		to := constraintTable("_new", "`id` > 0", schema.Cascade, false)
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		require.Len(t, changes, 6)
	})
	t.Run("index comment change preserves foreign key replacement", func(t *testing.T) {
		from := constraintTable(vitessSuffix, "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, false)
		from.Indexes[0].Attrs = append(from.Indexes[0].Attrs, &schema.Comment{Text: "before"})
		to.Indexes[0].Attrs = append(to.Indexes[0].Attrs, &schema.Comment{Text: "after"})
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		require.Len(t, changes, 4)
		require.IsType(t, &schema.DropIndex{}, changes[0])
		require.IsType(t, &schema.AddIndex{}, changes[1])
		require.IsType(t, &schema.DropForeignKey{}, changes[2])
		require.IsType(t, &schema.AddForeignKey{}, changes[3])
	})
	t.Run("real modifications", func(t *testing.T) {
		from := constraintTable(vitessSuffix, "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` >= 0", schema.Restrict, false)
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		require.Len(t, changes, 6)
		plan, err := DefaultPlan.PlanChanges(context.Background(), "constraint names", []schema.Change{
			&schema.ModifyTable{T: from, Changes: changes},
		})
		require.NoError(t, err)
		require.Len(t, plan.Changes, 1)
		require.Contains(t, plan.Changes[0].Cmd, "DROP CONSTRAINT `children_id_positive"+vitessSuffix+"`")
		require.Contains(t, plan.Changes[0].Cmd, "DROP FOREIGN KEY `children_parent_fk"+vitessSuffix+"`")
		require.Contains(t, plan.Changes[0].Cmd, "ADD CONSTRAINT `children_id_positive`")
		require.Contains(t, plan.Changes[0].Cmd, "ADD CONSTRAINT `children_parent_fk`")
	})
	t.Run("real drops", func(t *testing.T) {
		from := constraintTable(vitessSuffix, "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, false)
		to.ForeignKeys, to.Indexes, to.Attrs = nil, nil, nil
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		plan, err := DefaultPlan.PlanChanges(context.Background(), "constraint names", []schema.Change{
			&schema.ModifyTable{T: from, Changes: changes},
		})
		require.NoError(t, err)
		require.Len(t, plan.Changes, 1)
		require.Contains(t, plan.Changes[0].Cmd, "DROP CONSTRAINT `children_id_positive"+vitessSuffix+"`")
		require.Contains(t, plan.Changes[0].Cmd, "DROP FOREIGN KEY `children_parent_fk"+vitessSuffix+"`")
	})
	t.Run("real adds", func(t *testing.T) {
		from := constraintTable("", "`id` > 0", schema.Cascade, false)
		from.ForeignKeys, from.Indexes, from.Attrs = nil, nil, nil
		to := constraintTable("", "`id` > 0", schema.Cascade, false)
		changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames(ConstraintNamesIgnoreVitess))
		require.NoError(t, err)
		plan, err := DefaultPlan.PlanChanges(context.Background(), "constraint names", []schema.Change{
			&schema.ModifyTable{T: from, Changes: changes},
		})
		require.NoError(t, err)
		require.Len(t, plan.Changes, 1)
		require.Contains(t, plan.Changes[0].Cmd, "ADD CONSTRAINT `children_id_positive`")
		require.Contains(t, plan.Changes[0].Cmd, "ADD CONSTRAINT `children_parent_fk`")
	})
	t.Run("invalid strategy", func(t *testing.T) {
		from := constraintTable("", "`id` > 0", schema.Cascade, false)
		to := constraintTable("", "`id` > 0", schema.Cascade, false)
		_, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), DiffConstraintNames("invalid"))
		require.EqualError(t, err, `mysql: unknown constraint-name strategy "invalid"`)
	})
}

func TestDiffConstraintNames_Compose(t *testing.T) {
	var extension schemahcl.DefaultExtension
	extension.Extra.Children = []*schemahcl.Resource{{
		Type:  "constraint_names",
		Attrs: []*schemahcl.Attr{{K: "strategy", V: cty.StringVal("ignore_all")}},
	}}
	opts := &schema.DiffOptions{Extra: &extension}
	DiffConstraintNames(ConstraintNamesIgnoreVitess)(opts)
	extra, err := mysqlDiffOptions(opts)
	require.NoError(t, err)
	require.Equal(t, ConstraintNamesIgnoreVitess, extra.ConstraintNames.Strategy)
	require.Same(t, &extension, opts.Extra.(*DiffOptions).extra)
}

func TestDiff_ConstraintNamesHCL(t *testing.T) {
	var extra schemahcl.DefaultExtension
	extra.Extra.Children = []*schemahcl.Resource{{
		Type:  "constraint_names",
		Attrs: []*schemahcl.Attr{{K: "strategy", V: cty.StringVal("ignore_vitess")}},
	}}
	from := constraintTable(vitessSuffix, "`id` > 0", schema.Cascade, false)
	to := constraintTable("", "`id` > 0", schema.Cascade, false)
	changes, err := DefaultDiff.TableDiff(from, to, schema.DiffNormalized(), func(opts *schema.DiffOptions) {
		opts.Extra = extra
	})
	require.NoError(t, err)
	require.Empty(t, changes)
}

func TestParseVitessConstraintName(t *testing.T) {
	for _, tt := range []struct {
		name, want string
	}{
		{name: "check1", want: "check1"},
		{name: "check1_7no794p1x6zw6je1gfqmt7bca", want: "check1"},
		{name: "children_chk_1", want: "chk_1"},
		{name: "children_chk_1_7no794p1x6zw6je1gfqmt7bca", want: "chk_1"},
		{name: "chk_1_7no794p1x6zw6je1gfqmt7bca", want: "chk_1"},
		{name: "children_ibfk_1", want: "ibfk_1"},
		{name: "children_ibfk_1_7no794p1x6zw6je1gfqmt7bca", want: "ibfk_1"},
		{name: "ibfk_1_7no794p1x6zw6je1gfqmt7bca", want: "ibfk_1"},
		{name: "check1_7no794p1x6zw6je1gfqmt7bcas", want: "check1_7no794p1x6zw6je1gfqmt7bcas"},
		{name: "check1_7NO794P1X6ZW6JE1GFQMT7BCA", want: "check1_7NO794P1X6ZW6JE1GFQMT7BCA"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, parseVitessConstraintName("children", tt.name).base)
		})
	}
	longTable := strings.Repeat("children", 8)
	for _, kind := range []string{"ibfk_1", "chk_1"} {
		desired := longTable + "_" + kind
		live := desired[:mysqlMaxConstraintNameLen-vitessConstraintSuffixLen] + vitessSuffix
		require.True(t, vitessConstraintNamesMatch(longTable, live, desired))
	}
}

func renameConstraints(table *schema.Table, foreignKey, check string) {
	table.ForeignKeys[0].Symbol = foreignKey
	table.Indexes[0].Name = foreignKey
	table.Attrs[0].(*schema.Check).Name = check
}

func constraintTable(suffix, checkExpr string, deleteAction schema.ReferenceOption, addColumn bool) *schema.Table {
	s := schema.New("dev")
	parent := schema.NewTable("parents").SetSchema(s)
	parentID := schema.NewIntColumn("id", TypeBigInt)
	parent.AddColumns(parentID)

	child := schema.NewTable("children").SetSchema(s)
	childID := schema.NewIntColumn("id", TypeBigInt)
	parentRef := schema.NewIntColumn("parent_id", TypeBigInt)
	child.AddColumns(childID, parentRef)
	if addColumn {
		child.AddColumns(schema.NewBoolColumn("intended", TypeBool))
	}
	fkName := "children_parent_fk" + suffix
	child.AddIndexes(schema.NewIndex(fkName).AddParts(schema.NewIndexPart().SetColumn(parentRef)))
	child.AddForeignKeys(schema.NewForeignKey(fkName).
		SetTable(child).
		AddColumns(parentRef).
		SetRefTable(parent).
		AddRefColumns(parentID).
		SetOnUpdate(schema.NoAction).
		SetOnDelete(deleteAction))
	child.AddAttrs(schema.NewCheck().SetName("children_id_positive" + suffix).SetExpr(checkExpr))
	return child
}
