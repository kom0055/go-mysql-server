// Copyright 2023 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package memo

import (
	"fmt"
	"strings"

	"github.com/dolthub/go-mysql-server/sql/expression"
	"github.com/dolthub/go-mysql-server/sql/transform"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/plan"
)

// relProps are relational attributes shared by all plans in an expression
// group (see: ExprGroup).
type relProps struct {
	grp *ExprGroup

	fds          *sql.FuncDepSet
	outputCols   sql.ColSet
	inputTables  sql.FastIntSet
	outputTables sql.FastIntSet
	tableNodes   []plan.TableIdNode

	card float64

	Distinct distinctOp
	limit    sql.Expression
	sort     sql.SortFields
}

func newRelProps(rel RelExpr) *relProps {
	p := &relProps{
		grp: rel.Group(),
	}
	switch r := rel.(type) {
	case *Max1Row:
		p.populateFds()
	case SourceRel:
		p.outputCols = r.TableIdNode().Columns()
	default:
	}

	p.populateFds()
	p.populateOutputTables()
	p.populateInputTables()
	return p
}

// idxExprsColumns returns the column names used in an index's expressions.
// TODO: this is unstable as long as periods in Index.Expressions()
// identifiers are ambiguous.
func idxExprsColumns(idx sql.Index) []string {
	columns := make([]string, len(idx.Expressions()))
	for i, e := range idx.Expressions() {
		parts := strings.Split(e, ".")
		columns[i] = strings.ToLower(parts[1])
	}
	return columns
}

func (p *relProps) populateFds() {
	var fds *sql.FuncDepSet
	switch rel := p.grp.First.(type) {
	case JoinRel:
		jp := rel.JoinPrivate()
		switch {
		case jp.Op.IsDegenerate():
			fds = sql.NewCrossJoinFDs(jp.Left.RelProps.FuncDeps(), jp.Right.RelProps.FuncDeps())
		case jp.Op.IsLeftOuter():
			fds = sql.NewLeftJoinFDs(jp.Left.RelProps.FuncDeps(), jp.Right.RelProps.FuncDeps(), getEquivs(jp.Filter))
		default:
			fds = sql.NewInnerJoinFDs(jp.Left.RelProps.FuncDeps(), jp.Right.RelProps.FuncDeps(), getEquivs(jp.Filter))
		}
	case *Max1Row:
		all := rel.Child.RelProps.FuncDeps().All()
		notNull := rel.Child.RelProps.FuncDeps().NotNull()
		fds = sql.NewMax1RowFDs(all, notNull)
	case SourceRel:
		n := rel.TableIdNode()
		all := n.Columns()

		sch := allTableCols(rel)
		var notNull sql.ColSet
		j := 0
		for id, hasNext := all.Next(1); hasNext; id, hasNext = all.Next(id + 1) {
			if !sch[j].Nullable {
				notNull.Add(id)
			}
			j++
		}

		var indexes []sql.Index
		switch n := rel.(type) {
		case *TableAlias:
			rt, ok := n.Table.Child.(sql.TableNode)
			if !ok {
				break
			}
			table := rt.UnderlyingTable()
			indexableTable, ok := table.(sql.IndexAddressableTable)
			if !ok {
				break
			}
			indexes, _ = indexableTable.GetIndexes(rel.Group().m.Ctx)
		case *TableScan:
			table := n.Table.(sql.TableNode).UnderlyingTable()
			indexableTable, ok := table.(sql.IndexAddressableTable)
			if !ok {
				break
			}
			indexes, _ = indexableTable.GetIndexes(rel.Group().m.Ctx)
		default:
		}

		firstCol, _ := all.Next(1)

		var strictKeys []sql.ColSet
		var laxKeys []sql.ColSet
		var indexesNorm []*Index
		for _, idx := range indexes {
			// strict if primary key or all nonNull and unique
			columns := idxExprsColumns(idx)
			strict := true
			normIdx := &Index{idx: idx, order: make([]sql.ColumnId, len(columns))}
			for i, c := range columns {
				ord := sch.IndexOfColName(strings.ToLower(c))
				idOffset := firstCol + sql.ColumnId(ord)
				colId, _ := all.Next(idOffset)
				if colId == 0 {
					panic(fmt.Sprintf("colset invalid for join leaf: %s missing %d", all.String(), firstCol+sql.ColumnId(ord)))
				}
				normIdx.set.Add(colId)
				normIdx.order[i] = colId
				if !notNull.Contains(colId) {
					strict = false
				}
			}
			if !idx.IsUnique() {
				// not an FD
			} else if strict {
				strictKeys = append(strictKeys, normIdx.set)
			} else {
				laxKeys = append(laxKeys, normIdx.set)
			}
			indexesNorm = append(indexesNorm, normIdx)
		}
		rel.SetIndexes(indexesNorm)
		fds = sql.NewTablescanFDs(all, strictKeys, laxKeys, notNull)
	case *Filter:
		var notNull sql.ColSet
		var constant sql.ColSet
		var equiv [][2]sql.ColumnId
		for _, f := range rel.Filters {
			switch f := f.(type) {
			case *expression.Equals:
				if l, ok := f.Left().(*expression.GetField); ok {
					switch r := f.Right().(type) {
					case *expression.GetField:
						equiv = append(equiv, [2]sql.ColumnId{l.Id(), r.Id()})
					case *expression.Literal:
						constant.Add(l.Id())
						if r.Value() != nil {
							notNull.Add(l.Id())
						}
					}
				}
				if r, ok := f.Right().(*expression.GetField); ok {
					switch l := f.Left().(type) {
					case *expression.GetField:
						equiv = append(equiv, [2]sql.ColumnId{l.Id(), r.Id()})
					case *expression.Literal:
						constant.Add(r.Id())
						if l.Value() != nil {
							notNull.Add(r.Id())
						}
					}
				}
			case *expression.Not:
				child, ok := f.Child.(*expression.IsNull)
				if ok {
					col, ok := child.Child.(*expression.GetField)
					if ok {
						notNull.Add(col.Id())
					}
				}
			}
		}
		fds = sql.NewFilterFDs(rel.Child.RelProps.FuncDeps(), notNull, constant, equiv)
	case *Project:
		var projCols sql.ColSet
		for _, e := range rel.Projections {
			cols, _, _ := getExprScalarProps(e)
			projCols = projCols.Union(cols)
		}
		fds = sql.NewProjectFDs(rel.Child.RelProps.FuncDeps(), projCols, false)
	case *Distinct:
		fds = sql.NewProjectFDs(rel.Child.RelProps.FuncDeps(), rel.Child.RelProps.FuncDeps().All(), true)
	default:
		panic(fmt.Sprintf("unsupported relProps type: %T", rel))
	}
	p.fds = fds
}

// getExprScalarProps returns bitsets of the column and table references,
// and whether the expression is null rejecting.
func getExprScalarProps(e sql.Expression) (sql.ColSet, sql.FastIntSet, bool) {
	var cols sql.ColSet
	var tables sql.FastIntSet
	nullRej := true
	transform.InspectExpr(e, func(e sql.Expression) bool {
		switch e := e.(type) {
		case *expression.GetField:
			cols.Add(e.Id())
			tables.Add(int(e.TableId()))
		case *expression.NullSafeEquals:
			nullRej = false
		}
		return false
	})
	return cols, tables, nullRej
}

// allTableCols returns the full schema of a table ignoring
// declared projections.
func allTableCols(rel SourceRel) sql.Schema {
	var table sql.Table
	switch rel := rel.(type) {
	case *TableAlias:
		rt, ok := rel.Table.Child.(*plan.ResolvedTable)
		if !ok {
			break
		}
		table = rt.UnderlyingTable()
	case *TableScan:
		table = rel.Table.(sql.TableNode).UnderlyingTable()
	default:
		return rel.OutputCols()
	}
	projTab, ok := table.(sql.PrimaryKeyTable)
	if !ok {
		return rel.OutputCols()
	}

	sch := projTab.PrimaryKeySchema().Schema
	ret := make(sql.Schema, len(sch))
	for i, c := range sch {
		// TODO: generation_expression
		ret[i] = &sql.Column{
			Name:           c.Name,
			Type:           c.Type,
			Default:        c.Default,
			AutoIncrement:  c.AutoIncrement,
			Nullable:       c.Nullable,
			Source:         rel.Name(),
			DatabaseSource: c.DatabaseSource,
			PrimaryKey:     c.PrimaryKey,
			Comment:        c.Comment,
			Extra:          c.Extra,
		}
	}
	return ret

}

// getEquivs collects column equivalencies in the format sql.EquivSet expects.
func getEquivs(filters []sql.Expression) [][2]sql.ColumnId {
	var ret [][2]sql.ColumnId
	for _, f := range filters {
		var l, r *expression.GetField
		switch f := f.(type) {
		case *expression.Equals:
			l, _ = f.Left().(*expression.GetField)
			r, _ = f.Right().(*expression.GetField)
		case *expression.NullSafeEquals:
			l, _ = f.Left().(*expression.GetField)
			r, _ = f.Right().(*expression.GetField)
		}
		if l != nil && r != nil {
			ret = append(ret, [2]sql.ColumnId{l.Id(), r.Id()})
		}
	}
	return ret
}

func (p *relProps) FuncDeps() *sql.FuncDepSet {
	if p.fds == nil {
		p.populateFds()
	}
	return p.fds
}

// populateOutputTables initializes the bitmap indicating which tables'
// attributes are available outputs from the ExprGroup
func (p *relProps) populateOutputTables() {
	switch n := p.grp.First.(type) {
	case SourceRel:
		p.outputTables = sql.NewFastIntSet(int(n.TableIdNode().Id()))
		p.tableNodes = []plan.TableIdNode{n.TableIdNode()}
	case *AntiJoin:
		p.outputTables = n.Left.RelProps.OutputTables()
		p.tableNodes = n.Left.RelProps.TableIdNodes()
	case *SemiJoin:
		p.outputTables = n.Left.RelProps.OutputTables()
		p.tableNodes = n.Left.RelProps.TableIdNodes()
	case *Distinct:
		p.outputTables = n.Child.RelProps.OutputTables()
		p.tableNodes = n.Child.RelProps.TableIdNodes()
	case *Project:
		p.outputTables = n.Child.RelProps.OutputTables()
		p.tableNodes = n.Child.RelProps.TableIdNodes()
	case *Filter:
		p.outputTables = n.Child.RelProps.OutputTables()
		p.tableNodes = n.Child.RelProps.TableIdNodes()
	case *Max1Row:
		p.outputTables = n.Child.RelProps.OutputTables()
		p.tableNodes = n.Child.RelProps.TableIdNodes()
	case JoinRel:
		p.outputTables = n.JoinPrivate().Left.RelProps.OutputTables().Union(n.JoinPrivate().Right.RelProps.OutputTables())
		leftNodeCnt := len(n.JoinPrivate().Left.RelProps.tableNodes)
		rightNodeCnt := len(n.JoinPrivate().Right.RelProps.tableNodes)
		p.tableNodes = make([]plan.TableIdNode, leftNodeCnt+rightNodeCnt)
		copy(p.tableNodes, n.JoinPrivate().Left.RelProps.tableNodes)
		copy(p.tableNodes[leftNodeCnt:], n.JoinPrivate().Right.RelProps.tableNodes)
	default:
		panic(fmt.Sprintf("unhandled type: %T", n))
	}
}

// populateInputTables initializes the bitmap indicating which tables
// are input into this ExprGroup. This is used to enforce join order
// hinting for semi joins.
func (p *relProps) populateInputTables() {
	switch n := p.grp.First.(type) {
	case SourceRel:
		p.inputTables = sql.NewFastIntSet(int(p.grp.Id))
	case *Distinct:
		p.inputTables = n.Child.RelProps.InputTables()
	case *Project:
		p.inputTables = n.Child.RelProps.InputTables()
	case *Filter:
		p.inputTables = n.Child.RelProps.InputTables()
	case *Max1Row:
		p.inputTables = n.Child.RelProps.InputTables()
	case JoinRel:
		p.inputTables = n.JoinPrivate().Left.RelProps.InputTables().Union(n.JoinPrivate().Right.RelProps.InputTables())
	default:
		panic(fmt.Sprintf("unhandled type: %T", n))
	}
}

func (p *relProps) populateOutputCols() {
	p.outputCols = p.outputColsForRel(p.grp.Best)
}

func (p *relProps) outputColsForRel(r RelExpr) sql.ColSet {
	switch r := r.(type) {
	case *SemiJoin:
		return r.Left.RelProps.OutputCols()
	case *AntiJoin:
		return r.Left.RelProps.OutputCols()
	case *LookupJoin:
		if r.Op.IsPartial() {
			return r.Left.RelProps.OutputCols()
		} else {
			return r.JoinPrivate().Left.RelProps.OutputCols().Union(r.JoinPrivate().Right.RelProps.OutputCols())
		}
	case JoinRel:
		return r.JoinPrivate().Left.RelProps.OutputCols().Union(r.JoinPrivate().Right.RelProps.OutputCols())
	case *Distinct:
		return r.Child.RelProps.OutputCols()
	case *Project:
		return r.outputCols()
	case *Filter:
		return r.outputCols()
	case *Max1Row:
		return r.outputCols()
	case SourceRel:
		return r.TableIdNode().Columns()
	default:
		panic("unknown type")
	}
	return sql.ColSet{}
}

// OutputCols returns the output schema of a node
func (p *relProps) OutputCols() sql.ColSet {
	if p.outputCols.Empty() {
		if p.grp.Best == nil {
			return p.outputColsForRel(p.grp.First)
		}
		p.populateOutputCols()
	}
	return p.outputCols
}

// OutputTables returns a bitmap of tables in the output schema of this node.
func (p *relProps) OutputTables() sql.FastIntSet {
	return p.outputTables
}

// TableIdNodes returns a list of table id nodes in this relation
func (p *relProps) TableIdNodes() []plan.TableIdNode {
	return p.tableNodes
}

// InputTables returns a bitmap of tables input into this node.
func (p *relProps) InputTables() sql.FastIntSet {
	return p.inputTables
}

// sortedInputs returns true if a relation's inputs are sorted on the
// full output schema. The OrderedDistinct operator can be used in this
// case.
func sortedInputs(rel RelExpr) bool {
	switch r := rel.(type) {
	case *Max1Row:
		return true
	case *Project:
		if _, ok := r.Child.Best.(*Max1Row); ok {
			return true
		}
		inputs := sortedColsForRel(r.Child.Best)
		outputs := r.Projections
		i := 0
		j := 0
		for i < len(r.Projections) && j < len(inputs) {
			out := transform.ExpressionToColumn(outputs[i], plan.AliasSubqueryString(outputs[i]))
			in := inputs[j]
			// i -> output idx (distinct)
			// j -> input idx
			// want to find matches for all i where j_i <= j_i+1
			if strings.EqualFold(out.Name, in.Name) &&
				strings.EqualFold(out.Source, in.Source) {
				i++
			} else {
				// identical projections satisfied by same input
				j++
			}
		}
		return i == len(outputs)
	default:
		return false
	}
}

func sortedColsForRel(rel RelExpr) sql.Schema {
	switch r := rel.(type) {
	case *TableScan:
		tab, ok := r.Table.(sql.TableNode).UnderlyingTable().(sql.PrimaryKeyTable)
		if ok {
			ords := tab.PrimaryKeySchema().PkOrdinals
			var pks sql.Schema
			for _, i := range ords {
				pks = append(pks, tab.PrimaryKeySchema().Schema[i])
			}
			return pks
		}
	case *MergeJoin:
		var ret sql.Schema
		for _, e := range r.InnerScan.Idx.SqlIdx().Expressions() {
			// TODO columns can have "." characters, this will miss cases
			parts := strings.Split(e, ".")
			var name string
			if len(parts) == 2 {
				name = parts[1]
			} else {
				return nil
			}
			ret = append(ret, &sql.Column{
				Name:     strings.ToLower(name),
				Source:   strings.ToLower(r.InnerScan.Idx.SqlIdx().Table()),
				Nullable: true},
			)
		}
		return ret
	case JoinRel:
		return sortedColsForRel(r.JoinPrivate().Left.Best)
	case *Project:
		// TODO remove projections from sortedColsForRel(n.child.best)
		return nil
	case *TableAlias:
		rt, ok := r.Table.Child.(*plan.ResolvedTable)
		if !ok {
			return nil
		}
		tab, ok := rt.Table.(sql.PrimaryKeyTable)
		if ok {
			ords := tab.PrimaryKeySchema().PkOrdinals
			var pks sql.Schema
			for _, i := range ords {
				col := tab.PrimaryKeySchema().Schema[i].Copy()
				col.Source = r.Name()
				pks = append(pks, col)
			}
			return pks
		}
	default:
	}
	return nil
}
