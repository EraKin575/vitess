/*
Copyright 2022 The Vitess Authors.

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

package utils

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/sqlparser"
)

type TestingT interface {
	require.TestingT
	Helper()
}

type MySQLCompare struct {
	t                 TestingT
	MySQLConn, VtConn *mysql.Conn
}

func NewMySQLCompare(t TestingT, vtParams, mysqlParams mysql.ConnParams) (MySQLCompare, error) {
	ctx := context.Background()
	vtConn, err := mysql.Connect(ctx, &vtParams)
	if err != nil {
		return MySQLCompare{}, err
	}

	mysqlConn, err := mysql.Connect(ctx, &mysqlParams)
	if err != nil {
		return MySQLCompare{}, err
	}

	return MySQLCompare{
		t:         t,
		MySQLConn: mysqlConn,
		VtConn:    vtConn,
	}, nil
}

func (mcmp *MySQLCompare) AsT() *testing.T {
	return mcmp.t.(*testing.T)
}

func (mcmp *MySQLCompare) Close() {
	mcmp.VtConn.Close()
	mcmp.MySQLConn.Close()
}

// AssertMatches executes the given query on both Vitess and MySQL and make sure
// they have the same result set. The result set of Vitess is then matched with the given expectation.
func (mcmp *MySQLCompare) AssertMatches(query, expected string) {
	mcmp.t.Helper()
	qr := mcmp.Exec(query)
	got := fmt.Sprintf("%v", qr.Rows)
	diff := cmp.Diff(expected, got)
	if diff != "" {
		mcmp.t.Errorf("Query: %s (-want +got):\n%s\nGot:%s", query, diff, got)
	}
}

// SkipIfBinaryIsBelowVersion should be used instead of using utils.SkipIfBinaryIsBelowVersion(t,
// This is because we might be inside a Run block that has a different `t` variable
func (mcmp *MySQLCompare) SkipIfBinaryIsBelowVersion(majorVersion int, binary string) {
	SkipIfBinaryIsBelowVersion(mcmp.t.(*testing.T), majorVersion, binary)
}

// AssertMatchesAny ensures the given query produces any one of the expected results.
func (mcmp *MySQLCompare) AssertMatchesAny(query string, expected ...string) {
	mcmp.t.Helper()
	qr := mcmp.Exec(query)
	got := fmt.Sprintf("%v", qr.Rows)
	for _, e := range expected {
		diff := cmp.Diff(e, got)
		if diff == "" {
			return
		}
	}
	mcmp.t.Errorf("Query: %s (-want +got):\n%v\nGot:%s", query, expected, got)
}

// AssertMatchesAnyNoCompare ensures the given query produces any one of the expected results.
// This method does not compare the mysql and vitess results together
func (mcmp *MySQLCompare) AssertMatchesAnyNoCompare(query string, expected ...string) {
	mcmp.t.Helper()

	mQr, vQr := mcmp.ExecNoCompare(query)
	got := fmt.Sprintf("%v", mQr.Rows)
	valid := false
	for _, e := range expected {
		diff := cmp.Diff(e, got)
		if diff == "" {
			valid = true
			break
		}
	}
	if !valid {
		mcmp.t.Errorf("MySQL Query: %s (-want +got):\n%v\nGot:%s", query, expected, got)
	}
	valid = false

	got = fmt.Sprintf("%v", vQr.Rows)
	for _, e := range expected {
		diff := cmp.Diff(e, got)
		if diff == "" {
			valid = true
			break
		}
	}
	if !valid {
		mcmp.t.Errorf("Vitess Query: %s (-want +got):\n%v\nGot:%s", query, expected, got)
	}
}

// AssertContainsError executes the query on both Vitess and MySQL.
// Both clients need to return an error. The error of Vitess must be matching the given expectation.
func (mcmp *MySQLCompare) AssertContainsError(query, expected string) {
	mcmp.t.Helper()
	_, err := mcmp.ExecAllowAndCompareError(query, CompareOptions{})
	require.Error(mcmp.t, err)
	assert.ErrorContains(mcmp.t, err, expected, "actual error: %s", err.Error())
}

// AssertMatchesNoOrder executes the given query against both Vitess and MySQL.
// The test will be marked as failed if there is a mismatch between the two result sets.
func (mcmp *MySQLCompare) AssertMatchesNoOrder(query, expected string) {
	mcmp.t.Helper()
	qr := mcmp.Exec(query)
	if err := sqltypes.RowsEqualsStr(expected, qr.Rows); err != nil {
		mcmp.t.Errorf("for query [%s] %v", query, err)
	}
}

// AssertMatchesNoOrderInclColumnNames executes the given query against both Vitess and MySQL.
// The test will be marked as failed if there is a mismatch between the two result sets.
// This method also checks that the column names are the same and in the same order
func (mcmp *MySQLCompare) AssertMatchesNoOrderInclColumnNames(query, expected string) {
	mcmp.t.Helper()
	qr := mcmp.ExecWithColumnCompare(query)
	if err := sqltypes.RowsEqualsStr(expected, qr.Rows); err != nil {
		mcmp.t.Errorf("for query [%s] %v", query, err)
	}
}

// AssertIsEmpty executes the given query against both Vitess and MySQL and ensures
// their results match and are empty.
func (mcmp *MySQLCompare) AssertIsEmpty(query string) {
	mcmp.t.Helper()
	qr := mcmp.Exec(query)
	assert.Empty(mcmp.t, qr.Rows, "for query: "+query)
}

// AssertFoundRowsValue executes the given query against both Vitess and MySQL.
// The results of that query must match between Vitess and MySQL, otherwise the test will be
// marked as failed. Once the query is executed, the test checks the value of `found_rows`,
// which must match the given `count` argument.
func (mcmp *MySQLCompare) AssertFoundRowsValue(query, workload string, count int) {
	mcmp.Exec(query)

	qr := mcmp.Exec("select found_rows()")
	got := fmt.Sprintf("%v", qr.Rows)
	want := fmt.Sprintf(`[[INT64(%d)]]`, count)
	assert.Equalf(mcmp.t, want, got, "Workload: %s\nQuery:%s\n", workload, query)
}

// AssertMatchesNoCompare compares the record of mysql and vitess separately and not with each other.
func (mcmp *MySQLCompare) AssertMatchesNoCompare(query, mExp string, vExp string) {
	mcmp.t.Helper()
	mQr, vQr := mcmp.ExecNoCompare(query)
	got := fmt.Sprintf("%v", mQr.Rows)
	diff := cmp.Diff(mExp, got)
	if diff != "" {
		mcmp.t.Errorf("MySQL Query: %s (-want +got):\n%s\nGot:%s", query, diff, got)
	}
	got = fmt.Sprintf("%v", vQr.Rows)
	diff = cmp.Diff(vExp, got)
	if diff != "" {
		mcmp.t.Errorf("Vitess Query: %s (-want +got):\n%s\nGot:%s", query, diff, got)
	}
}

// Exec executes the given query against both Vitess and MySQL and compares
// the two result set. If there is a mismatch, the difference will be printed and the
// test will fail. If the query produces an error in either Vitess or MySQL, the test
// will be marked as failed.
// The result set of Vitess is returned to the caller.
func (mcmp *MySQLCompare) Exec(query string) *sqltypes.Result {
	mcmp.t.Helper()
	vtQr, err := mcmp.VtConn.ExecuteFetch(query, 1000, true)
	require.NoError(mcmp.t, err, "[Vitess Error] for query: "+query)

	mysqlQr, err := mcmp.MySQLConn.ExecuteFetch(query, 1000, true)
	require.NoError(mcmp.t, err, "[MySQL Error] for query: "+query)
	CompareVitessAndMySQLResults(mcmp.t, query, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{})
	return vtQr
}

// ExecMulti executes the given queries against both Vitess and MySQL and compares
// the result sets. If there is a mismatch, the difference will be printed and the
// test will fail. If the query produces an error in either Vitess or MySQL, the test
// will be marked as failed.
// The result sets of Vitess are returned to the caller.
func (mcmp *MySQLCompare) ExecMulti(sql string) []*sqltypes.Result {
	mcmp.t.Helper()
	stmts, err := sqlparser.NewTestParser().SplitStatementToPieces(sql)
	require.NoError(mcmp.t, err)
	vtQr, vtMore, err := mcmp.VtConn.ExecuteFetchMulti(sql, 1000, true)
	require.NoError(mcmp.t, err, "[Vitess Error] for sql: "+sql)

	mysqlQr, mysqlMore, err := mcmp.MySQLConn.ExecuteFetchMulti(sql, 1000, true)
	require.NoError(mcmp.t, err, "[MySQL Error] for sql: "+sql)
	sql = stmts[0]
	CompareVitessAndMySQLResults(mcmp.t, sql, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{})
	if vtMore != mysqlMore {
		mcmp.AsT().Errorf("Vitess and MySQL have different More flags: %v vs %v", vtMore, mysqlMore)
	}

	results := []*sqltypes.Result{vtQr}
	idx := 1
	for vtMore {
		sql = stmts[idx]
		idx++
		vtQr, vtMore, _, err = mcmp.VtConn.ReadQueryResult(1000, true)
		require.NoError(mcmp.t, err, "[Vitess Error] for sql: "+sql)

		mysqlQr, mysqlMore, _, err = mcmp.MySQLConn.ReadQueryResult(1000, true)
		require.NoError(mcmp.t, err, "[MySQL Error] for sql: "+sql)
		CompareVitessAndMySQLResults(mcmp.t, sql, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{})
		if vtMore != mysqlMore {
			mcmp.AsT().Errorf("Vitess and MySQL have different More flags: %v vs %v", vtMore, mysqlMore)
		}
		results = append(results, vtQr)
	}

	return results
}

// ExecMultiAllowError executes the given queries against both Vitess and MySQL and compares
// the result sets and errors.
func (mcmp *MySQLCompare) ExecMultiAllowError(sql string) {
	mcmp.t.Helper()
	stmts, err := sqlparser.NewTestParser().SplitStatementToPieces(sql)
	require.NoError(mcmp.t, err)
	vtQr, vtMore, vtErr := mcmp.VtConn.ExecuteFetchMulti(sql, 1000, true)

	mysqlQr, mysqlMore, mysqlErr := mcmp.MySQLConn.ExecuteFetchMulti(sql, 1000, true)
	sql = stmts[0]
	compareVitessAndMySQLErrors(mcmp.t, vtErr, mysqlErr)
	if vtErr == nil && mysqlErr == nil {
		CompareVitessAndMySQLResults(mcmp.t, sql, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{})
	}
	if vtMore != mysqlMore {
		mcmp.AsT().Errorf("Vitess and MySQL have different More flags: %v vs %v", vtMore, mysqlMore)
	}

	idx := 1
	for vtMore {
		sql = stmts[idx]
		idx++
		vtQr, vtMore, _, vtErr = mcmp.VtConn.ReadQueryResult(1000, true)

		mysqlQr, mysqlMore, _, mysqlErr = mcmp.MySQLConn.ReadQueryResult(1000, true)
		compareVitessAndMySQLErrors(mcmp.t, vtErr, mysqlErr)
		if vtErr == nil && mysqlErr == nil {
			CompareVitessAndMySQLResults(mcmp.t, sql, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{})
		}
		if vtMore != mysqlMore {
			mcmp.AsT().Errorf("Vitess and MySQL have different More flags: %v vs %v", vtMore, mysqlMore)
		}
	}
}

// ExecVitessAndMySQLDifferentQueries executes Vitess and MySQL with the queries provided.
func (mcmp *MySQLCompare) ExecVitessAndMySQLDifferentQueries(vtQ, mQ string) *sqltypes.Result {
	mcmp.t.Helper()
	vtQr, err := mcmp.VtConn.ExecuteFetch(vtQ, 1000, true)
	require.NoError(mcmp.t, err, "[Vitess Error] for query: "+vtQ)

	mysqlQr, err := mcmp.MySQLConn.ExecuteFetch(mQ, 1000, true)
	require.NoError(mcmp.t, err, "[MySQL Error] for query: "+mQ)
	CompareVitessAndMySQLResults(mcmp.t, vtQ, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{})
	return vtQr
}

// ExecAssert is the same as Exec, but it only does assertions, it won't FailNow
func (mcmp *MySQLCompare) ExecAssert(query string) *sqltypes.Result {
	mcmp.t.Helper()
	vtQr, err := mcmp.VtConn.ExecuteFetch(query, 1000, true)
	assert.NoError(mcmp.t, err, "[Vitess Error] for query: "+query)

	mysqlQr, err := mcmp.MySQLConn.ExecuteFetch(query, 1000, true)
	assert.NoError(mcmp.t, err, "[MySQL Error] for query: "+query)
	CompareVitessAndMySQLResults(mcmp.t, query, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{})
	return vtQr
}

// ExecNoCompare executes the query on vitess and mysql but does not compare the result with each other.
func (mcmp *MySQLCompare) ExecNoCompare(query string) (*sqltypes.Result, *sqltypes.Result) {
	mcmp.t.Helper()
	vtQr, err := mcmp.VtConn.ExecuteFetch(query, 1000, true)
	require.NoError(mcmp.t, err, "[Vitess Error] for query: "+query)

	mysqlQr, err := mcmp.MySQLConn.ExecuteFetch(query, 1000, true)
	require.NoError(mcmp.t, err, "[MySQL Error] for query: "+query)
	return mysqlQr, vtQr
}

// ExecWithColumnCompare executes the given query against both Vitess and MySQL and compares
// the two result set. If there is a mismatch, the difference will be printed and the
// test will fail. If the query produces an error in either Vitess or MySQL, the test
// will be marked as failed.
// The result set of Vitess is returned to the caller.
func (mcmp *MySQLCompare) ExecWithColumnCompare(query string) *sqltypes.Result {
	mcmp.t.Helper()
	vtQr, err := mcmp.VtConn.ExecuteFetch(query, 1000, true)
	require.NoError(mcmp.t, err, "[Vitess Error] for query: "+query)

	mysqlQr, err := mcmp.MySQLConn.ExecuteFetch(query, 1000, true)
	require.NoError(mcmp.t, err, "[MySQL Error] for query: "+query)
	CompareVitessAndMySQLResults(mcmp.t, query, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{CompareColumnNames: true})
	return vtQr
}

// ExecAllowAndCompareError executes the query against both Vitess and MySQL.
// The test will pass if:
//   - MySQL and Vitess both agree that there is an error
//   - MySQL and Vitess did not find an error, but their results are matching
//
// The result set and error produced by Vitess are returned to the caller.
// If the Vitess and MySQL error are both nil, but the results do not match,
// the mismatched results are instead returned as an error, as well as the Vitess result set
func (mcmp *MySQLCompare) ExecAllowAndCompareError(query string, opts CompareOptions) (*sqltypes.Result, error) {
	mcmp.t.Helper()
	vtQr, vtErr := mcmp.VtConn.ExecuteFetch(query, 1000, true)
	mysqlQr, mysqlErr := mcmp.MySQLConn.ExecuteFetch(query, 1000, true)
	compareVitessAndMySQLErrors(mcmp.t, vtErr, mysqlErr)

	// Since we allow errors, we don't want to compare results if one of the client failed.
	// Vitess and MySQL should always be agreeing whether the query returns an error or not.
	if vtErr == nil && mysqlErr == nil {
		vtErr = CompareVitessAndMySQLResults(mcmp.t, query, mcmp.VtConn, vtQr, mysqlQr, opts)
	}
	return vtQr, vtErr
}

// ExecAndIgnore executes the query against both Vitess and MySQL.
// Errors and results difference are ignored.
func (mcmp *MySQLCompare) ExecAndIgnore(query string) (*sqltypes.Result, error) {
	mcmp.t.Helper()
	_, _ = mcmp.MySQLConn.ExecuteFetch(query, 1000, true)
	return mcmp.VtConn.ExecuteFetch(query, 1000, true)
}

func (mcmp *MySQLCompare) Run(name string, f func(mcmp *MySQLCompare)) {
	mcmp.AsT().Run(name, func(t *testing.T) {
		inner := &MySQLCompare{
			t:         t,
			MySQLConn: mcmp.MySQLConn,
			VtConn:    mcmp.VtConn,
		}
		f(inner)
	})
}

// ExecAllowError executes the query against both Vitess and MySQL.
// If there is no error, it compares the result
// Return any Vitess execution error without comparing the results.
func (mcmp *MySQLCompare) ExecAllowError(query string) (*sqltypes.Result, error) {
	mcmp.t.Helper()
	vtQr, vtErr := mcmp.VtConn.ExecuteFetch(query, 1000, true)
	if vtErr != nil {
		return nil, vtErr
	}
	mysqlQr, mysqlErr := mcmp.MySQLConn.ExecuteFetch(query, 1000, true)

	// Since we allow errors, we don't want to compare results if one of the client failed.
	// Vitess and MySQL should always be agreeing whether the query returns an error or not.
	if mysqlErr == nil {
		vtErr = CompareVitessAndMySQLResults(mcmp.t, query, mcmp.VtConn, vtQr, mysqlQr, CompareOptions{})
	}
	return vtQr, vtErr
}

func (mcmp *MySQLCompare) VExplain(query string) string {
	mcmp.t.Helper()
	vtQr, vtErr := mcmp.VtConn.ExecuteFetch("vexplain plan "+query, 1, true)
	require.NoError(mcmp.t, vtErr, "[Vitess Error] for query: "+query)
	return vtQr.Rows[0][0].ToString()
}
