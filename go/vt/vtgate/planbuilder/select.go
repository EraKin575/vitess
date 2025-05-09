/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package planbuilder

import (
	"fmt"

	"vitess.io/vitess/go/vt/key"
	querypb "vitess.io/vitess/go/vt/proto/query"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/engine"
	"vitess.io/vitess/go/vt/vtgate/evalengine"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/operators"
	"vitess.io/vitess/go/vt/vtgate/planbuilder/plancontext"
	"vitess.io/vitess/go/vt/vtgate/semantics"
)

func gen4SelectStmtPlanner(
	query string,
	plannerVersion querypb.ExecuteOptions_PlannerVersion,
	stmt sqlparser.SelectStatement,
	reservedVars *sqlparser.ReservedVars,
	vschema plancontext.VSchema,
) (*planResult, error) {
	sel, isSel := stmt.(*sqlparser.Select)
	if isSel {
		// handle dual table for processing at vtgate.
		p, err := handleDualSelects(sel, vschema)
		if err != nil {
			return nil, err
		}
		if p != nil {
			used := "dual"
			keyspace, ksErr := vschema.SelectedKeyspace()
			if ksErr == nil {
				// we are just getting the ks to log the correct table use.
				// no need to fail this if we can't find the default keyspace
				used = keyspace.Name + ".dual"
			}
			return newPlanResult(p, used), nil
		}

		if sel.SQLCalcFoundRows && sel.Limit != nil {
			return gen4planSQLCalcFoundRows(vschema, sel, query, reservedVars)
		}
		// if there was no limit, we can safely ignore the SQLCalcFoundRows directive
		sel.SQLCalcFoundRows = false
	}

	getPlan := func(selStatement sqlparser.SelectStatement) (engine.Primitive, []string, error) {
		return newBuildSelectPlan(selStatement, reservedVars, vschema, plannerVersion)
	}

	plan, tablesUsed, err := getPlan(stmt)
	if err != nil {
		return nil, err
	}

	if shouldRetryAfterPredicateRewriting(plan) {
		// by transforming the predicates to CNF, the planner will sometimes find better plans
		// TODO: this should move to the operator side of planning
		prim2, tablesUsed := gen4PredicateRewrite(stmt, getPlan)
		if prim2 != nil {
			return newPlanResult(prim2, tablesUsed...), nil
		}
	}

	if !isSel {
		return newPlanResult(plan, tablesUsed...), nil
	}

	// this is done because engine.Route doesn't handle the empty result well
	// if it doesn't find a shard to send the query to.
	// All other engine primitives can handle this, so we only need it when
	// Route is the last (and only) instruction before the user sees a result
	if isOnlyDual(sel) || (sel.GroupBy == nil && sel.SelectExprs.AllAggregation()) {
		switch prim := plan.(type) {
		case *engine.Route:
			prim.NoRoutesSpecialHandling = true
		case *engine.VindexLookup:
			prim.SendTo.NoRoutesSpecialHandling = true
		}
	}
	return newPlanResult(plan, tablesUsed...), nil
}

func gen4planSQLCalcFoundRows(vschema plancontext.VSchema, sel *sqlparser.Select, query string, reservedVars *sqlparser.ReservedVars) (*planResult, error) {
	ksName := ""
	if ks, _ := vschema.SelectedKeyspace(); ks != nil {
		ksName = ks.Name
	}
	semTable, err := semantics.Analyze(sel, ksName, vschema)
	if err != nil {
		return nil, err
	}
	// record any warning as planner warning.
	vschema.PlannerWarning(semTable.Warning)

	plan, tablesUsed, err := buildSQLCalcFoundRowsPlan(query, sel, reservedVars, vschema)
	if err != nil {
		return nil, err
	}
	return newPlanResult(plan, tablesUsed...), nil
}

func buildSQLCalcFoundRowsPlan(
	originalQuery string,
	sel *sqlparser.Select,
	reservedVars *sqlparser.ReservedVars,
	vschema plancontext.VSchema,
) (engine.Primitive, []string, error) {
	limitPlan, _, err := newBuildSelectPlan(sel, reservedVars, vschema, Gen4)
	if err != nil {
		return nil, nil, err
	}

	statement2, reserved2, err := vschema.Environment().Parser().Parse2(originalQuery)
	if err != nil {
		return nil, nil, err
	}
	sel2 := statement2.(*sqlparser.Select)

	sel2.SQLCalcFoundRows = false
	sel2.OrderBy = nil
	sel2.Limit = nil

	countStar := &sqlparser.AliasedExpr{Expr: &sqlparser.CountStar{}}
	selectExprs := &sqlparser.SelectExprs{
		Exprs: []sqlparser.SelectExpr{countStar},
	}
	if sel2.GroupBy == nil && sel2.Having == nil {
		// if there is no grouping, we can use the same query and
		// just replace the SELECT sub-clause to have a single count(*)
		sel2.SetSelectExprs(countStar)
	} else {
		// when there is grouping, we have to move the original query into a derived table.
		//                       select id, sum(12) from user group by id =>
		// select count(*) from (select id, sum(12) from user group by id) t
		sel3 := &sqlparser.Select{
			SelectExprs: selectExprs,
			From: []sqlparser.TableExpr{
				&sqlparser.AliasedTableExpr{
					Expr: &sqlparser.DerivedTable{Select: sel2},
					As:   sqlparser.NewIdentifierCS("t"),
				},
			},
		}
		sel2 = sel3
	}

	reservedVars2 := sqlparser.NewReservedVars("vtg", reserved2)

	countPlan, tablesUsed, err := newBuildSelectPlan(sel2, reservedVars2, vschema, Gen4)
	if err != nil {
		return nil, nil, err
	}

	rb, ok := countPlan.(*engine.Route)
	if ok {
		// if our count query is an aggregation, we want the no-match result to still return a zero
		rb.NoRoutesSpecialHandling = true
	}
	return &engine.SQLCalcFoundRows{
		LimitPrimitive: limitPlan,
		CountPrimitive: countPlan,
	}, tablesUsed, nil
}

func gen4PredicateRewrite(stmt sqlparser.Statement, getPlan func(selStatement sqlparser.SelectStatement) (engine.Primitive, []string, error)) (engine.Primitive, []string) {
	rewritten, isSel := sqlparser.RewritePredicate(stmt).(sqlparser.SelectStatement)
	if !isSel {
		// Fail-safe code, should never happen
		return nil, nil
	}
	plan2, op, err := getPlan(rewritten)
	if err == nil && !shouldRetryAfterPredicateRewriting(plan2) {
		// we only use this new plan if it's better than the old one we got
		return plan2, op
	}
	return nil, nil
}

func newBuildSelectPlan(
	selStmt sqlparser.SelectStatement,
	reservedVars *sqlparser.ReservedVars,
	vschema plancontext.VSchema,
	version querypb.ExecuteOptions_PlannerVersion,
) (plan engine.Primitive, tablesUsed []string, err error) {
	ctx, err := plancontext.CreatePlanningContext(selStmt, reservedVars, vschema, version)
	if err != nil {
		return nil, nil, err
	}

	if ks, ok := ctx.SemTable.CanTakeSelectUnshardedShortcut(); ok {
		plan, tablesUsed, err = selectUnshardedShortcut(ctx, selStmt, ks)
		if err != nil {
			return nil, nil, err
		}
		setCommentDirectivesOnPlan(plan, selStmt)
		return plan, tablesUsed, err
	}

	if ctx.SemTable.NotUnshardedErr != nil {
		return nil, nil, ctx.SemTable.NotUnshardedErr
	}

	op, err := createSelectOperator(ctx, selStmt)
	if err != nil {
		return nil, nil, err
	}

	plan, err = transformToPrimitive(ctx, op)
	if err != nil {
		return nil, nil, err
	}

	return plan, operators.TablesUsed(op), nil
}

func createSelectOperator(ctx *plancontext.PlanningContext, selStmt sqlparser.SelectStatement) (operators.Operator, error) {
	err := queryRewrite(ctx, selStmt)
	if err != nil {
		return nil, err
	}

	return operators.PlanQuery(ctx, selStmt)
}

func isOnlyDual(sel *sqlparser.Select) bool {
	if sel.Where != nil || sel.GroupBy != nil || sel.Having != nil || sel.OrderBy != nil {
		// we can only deal with queries without any other subclauses - just SELECT and FROM, nothing else is allowed
		return false
	}

	if sel.Limit != nil {
		if sel.Limit.Offset != nil {
			return false
		}
		limit := sel.Limit.Rowcount
		switch limit := limit.(type) {
		case nil:
		case *sqlparser.Literal:
			if limit.Val == "0" {
				// A limit with any value other than zero can still return a row
				return false
			}
		default:
			return false
		}
	}

	if len(sel.From) > 1 {
		return false
	}
	table, ok := sel.From[0].(*sqlparser.AliasedTableExpr)
	if !ok {
		return false
	}
	tableName, ok := table.Expr.(sqlparser.TableName)

	return ok && tableName.Name.String() == "dual" && tableName.Qualifier.IsEmpty()
}

func shouldRetryAfterPredicateRewriting(plan engine.Primitive) bool {
	// if we have a I_S query, but have not found table_schema or table_name, let's try CNF
	switch eroute := plan.(type) {
	case *engine.Route:
		return eroute.Opcode == engine.DBA &&
			len(eroute.SysTableTableName) == 0 &&
			len(eroute.SysTableTableSchema) == 0
	default:
		return false
	}
}

func handleDualSelects(sel *sqlparser.Select, vschema plancontext.VSchema) (engine.Primitive, error) {
	if !isOnlyDual(sel) {
		return nil, nil
	}

	columns := sel.GetColumns()
	size := len(columns)
	exprs := make([]evalengine.Expr, size)
	cols := make([]string, size)
	var lockFunctions []*engine.LockFunc
	for i, e := range columns {
		expr, ok := e.(*sqlparser.AliasedExpr)
		if !ok {
			return nil, nil
		}
		var err error
		lFunc, isLFunc := expr.Expr.(*sqlparser.LockingFunc)
		if isLFunc {
			elem := &engine.LockFunc{Typ: expr.Expr.(*sqlparser.LockingFunc)}
			if lFunc.Name != nil {
				n, err := evalengine.Translate(lFunc.Name, &evalengine.Config{
					Collation:   vschema.ConnCollation(),
					Environment: vschema.Environment(),
				})
				if err != nil {
					return nil, err
				}
				elem.Name = n
			}
			lockFunctions = append(lockFunctions, elem)
			continue
		}
		if len(lockFunctions) > 0 {
			return nil, vterrors.VT12001(fmt.Sprintf("LOCK function and other expression: [%s] in same select query", sqlparser.String(expr)))
		}
		exprs[i], err = evalengine.Translate(expr.Expr, &evalengine.Config{
			Collation:   vschema.ConnCollation(),
			Environment: vschema.Environment(),
		})
		if err != nil {
			return nil, nil
		}
		cols[i] = expr.As.String()
		if cols[i] == "" {
			cols[i] = sqlparser.String(expr.Expr)
		}
	}
	if len(lockFunctions) > 0 {
		return buildLockingPrimitive(sel, vschema, lockFunctions)
	}
	return &engine.Projection{
		Exprs: exprs,
		Cols:  cols,
		Input: &engine.SingleRow{},
	}, nil
}

func buildLockingPrimitive(sel *sqlparser.Select, vschema plancontext.VSchema, lockFunctions []*engine.LockFunc) (engine.Primitive, error) {
	ks, err := vschema.FirstSortedKeyspace()
	if err != nil {
		return nil, err
	}
	buf := sqlparser.NewTrackedBuffer(sqlparser.FormatImpossibleQuery).WriteNode(sel)
	return &engine.Lock{
		Keyspace:          ks,
		TargetDestination: key.DestinationKeyspaceID{0},
		FieldQuery:        buf.String(),
		LockFunctions:     lockFunctions,
	}, nil
}
