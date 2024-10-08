/*
Copyright 2024 The Vitess Authors.

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

package tester

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/test/endtoend/cluster"
	"vitess.io/vitess/go/test/endtoend/utils"
	"vitess.io/vitess/go/vt/sqlparser"
	"vitess.io/vitess/go/vt/vtgate/vindexes"

	"github.com/vitessio/vt/go/data"
	"github.com/vitessio/vt/go/typ"
)

type (
	Tester struct {
		name string

		clusterInstance       *cluster.LocalProcessCluster
		vtParams, mysqlParams mysql.ConnParams
		curr                  utils.MySQLCompare

		olap        bool
		ksNames     []string
		vschema     vindexes.VSchema
		vschemaFile string
		vexplain    string

		// check expected error, use --error before the statement
		// we only care if an error is returned, not the exact error message.
		expectedErrs bool

		state *testerState

		reporter             Reporter
		alreadyWrittenTraces bool // we need to keep track of it is the first trace or not, to add commas in between traces

		qr QueryRunner
	}

	testerState struct {
		skip        bool
		skipBinary  string
		skipVersion int
		vitessOnly  bool
		mysqlOnly   bool
		reference   bool
	}

	QueryRunConfig struct {
		ast                      sqlparser.Statement
		vitess, mysql, reference bool
	}

	QueryRunner interface {
		runQuery(q data.Query, expectedErrs bool, cfg QueryRunConfig) error
	}

	QueryRunnerFactory interface {
		NewQueryRunner(reporter Reporter, handleCreateTable CreateTableHandler, comparer utils.MySQLCompare, cluster *cluster.LocalProcessCluster, table func(name string) (ks string, err error)) QueryRunner
		Close()
	}
)

func NewTester(
	name string,
	reporter Reporter,
	clusterInstance *cluster.LocalProcessCluster,
	vtParams, mysqlParams mysql.ConnParams,
	olap bool,
	ksNames []string,
	vschema vindexes.VSchema,
	vschemaFile string,
	factory QueryRunnerFactory,
) *Tester {
	t := &Tester{
		name:            name,
		reporter:        reporter,
		vtParams:        vtParams,
		mysqlParams:     mysqlParams,
		clusterInstance: clusterInstance,
		ksNames:         ksNames,
		vschema:         vschema,
		vschemaFile:     vschemaFile,
		olap:            olap,
		state:           &testerState{},
	}

	mcmp, err := utils.NewMySQLCompare(t.reporter, t.vtParams, t.mysqlParams)
	exitIf(err, "creating MySQLCompare")
	t.curr = mcmp
	createTableHandler := t.handleCreateTable
	if !t.autoVSchema() {
		createTableHandler = func(*sqlparser.CreateTable) func() { return func() {} }
	}
	t.qr = factory.NewQueryRunner(reporter, createTableHandler, mcmp, clusterInstance, t.findTable)

	return t
}

func (t *Tester) preProcess() {
	if t.olap {
		_, err := t.curr.VtConn.ExecuteFetch("set workload = 'olap'", 0, false)
		exitIf(err, "setting workload to olap by executing query")
	}
}

func (t *Tester) postProcess() {
	r, err := t.curr.MySQLConn.ExecuteFetch("show tables", 1000, true)
	exitIf(err, "running show tables")
	for _, row := range r.Rows {
		t.curr.Exec(fmt.Sprintf("drop table %s", row[0].ToString()))
	}
	t.curr.Close()
}

var PERM os.FileMode = 0755

func (t *Tester) getVschema() func() []byte {
	return func() []byte {
		httpClient := &http.Client{Timeout: 5 * time.Second}
		resp, err := httpClient.Get(t.clusterInstance.VtgateProcess.VSchemaURL)
		if err != nil {
			log.Errorf(err.Error())
			return nil
		}
		defer resp.Body.Close()
		res, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Errorf(err.Error())
			return nil
		}

		return res
	}
}

func (state *testerState) referenceNext() {
	if state.vitessOnly || state.mysqlOnly || state.skip {
		return
	}
	state.reference = true
}

func (state *testerState) shouldReference() bool {
	if state.reference {
		state.reference = false
		return true
	}
	return false
}

func (state *testerState) skipNext() {
	if state.vitessOnly || state.mysqlOnly || state.reference {
		return
	}
	state.skip = true
}

func (state *testerState) skipIfBelow(binary string, version int) {
	state.skipBinary = binary
	state.skipVersion = version
}

func (state *testerState) shouldSkip() bool {
	if state.skip {
		state.skip = false
		return true
	}

	if state.skipBinary != "" {
		okayToRun := utils.BinaryIsAtLeastAtVersion(state.skipVersion, state.skipBinary)
		state.skipBinary = ""
		return !okayToRun
	}

	return false
}

func (state *testerState) beginVitessOnly() error {
	if state.vitessOnly {
		return fmt.Errorf("nested vitess_only begin")
	}
	if state.mysqlOnly {
		return fmt.Errorf("cannot begin vitess_only within mysql_only")
	}
	state.vitessOnly = true
	return nil
}

func (state *testerState) endVitessOnly() error {
	if !state.vitessOnly {
		return fmt.Errorf("no vitess_only to end")
	}
	state.vitessOnly = false
	return nil
}

func (state *testerState) beginMySQLOnly() error {
	if state.mysqlOnly {
		return fmt.Errorf("nested mysql_only begin")
	}
	if state.vitessOnly {
		return fmt.Errorf("cannot begin mysql_only within vitess_only")
	}
	state.mysqlOnly = true
	return nil
}

func (state *testerState) endMySQLOnly() error {
	if !state.mysqlOnly {
		return fmt.Errorf("no vitess_only to end")
	}
	state.mysqlOnly = false
	return nil
}

func (t *Tester) Run() error {
	t.preProcess()
	if t.autoVSchema() {
		defer t.postProcess()
	}
	queries, err := data.LoadQueries(t.name)
	if err != nil {
		t.reporter.AddFailure(err)
		return err
	}

	for _, q := range queries {
		switch q.Type {
		case typ.Skip:
			t.state.skipNext()
		case typ.SkipIfBelowVersion:
			strs := strings.Split(q.Query, " ")
			if len(strs) != 3 {
				t.reporter.AddFailure(fmt.Errorf("incorrect syntax for typ.Q_SKIP_IF_BELOW_VERSION in: %v", q.Query))
				continue
			}
			v, err := strconv.Atoi(strs[2])
			if err != nil {
				t.reporter.AddFailure(err)
				continue
			}
			t.state.skipIfBelow(strs[1], v)
		case typ.Error:
			t.expectedErrs = true
		case typ.VExplain:
			strs := strings.Split(q.Query, " ")
			if len(strs) != 2 {
				t.reporter.AddFailure(fmt.Errorf("incorrect syntax for typ.VExplain in: %v", q.Query))
				continue
			}

			t.vexplain = strs[1]
		case typ.WaitForAuthoritative:
			t.waitAuthoritative(q.Query)
		case typ.Query:
			if t.vexplain != "" {
				result, err := t.curr.VtConn.ExecuteFetch(fmt.Sprintf("vexplain %s %s", t.vexplain, q.Query), -1, false)
				t.vexplain = ""
				if err != nil {
					t.reporter.AddFailure(err)
				}

				t.reporter.AddInfo(fmt.Sprintf("VExplain Output:\n %s\n", result.Rows[0][0].ToString()))
			}

			t.runQuery(q)
		case typ.RemoveFile:
			err = os.Remove(strings.TrimSpace(q.Query))
			if err != nil {
				return fmt.Errorf("failed to remove file: %w", err)
			}
		case typ.VitessOnly:
			err := vitessOrMySQLOnly(q.Query, t.state.beginVitessOnly, t.state.endVitessOnly)
			if err != nil {
				t.reporter.AddFailure(err)
			}
		case typ.MysqlOnly:
			err := vitessOrMySQLOnly(q.Query, t.state.beginMySQLOnly, t.state.endMySQLOnly)
			if err != nil {
				t.reporter.AddFailure(err)
			}
		case typ.Reference:
			t.state.referenceNext()
		default:
			t.reporter.AddFailure(fmt.Errorf("%s not supported", q.Type.String()))
		}
	}
	fmt.Printf("%s\n", t.reporter.Report())

	return nil
}

func vitessOrMySQLOnly(query string, begin, end func() error) error {
	strs := strings.Split(query, " ")
	if len(strs) != 2 {
		return fmt.Errorf("incorrect syntax in: %v", query)
	}

	switch strs[1] {
	case "begin":
		return begin()
	case "end":
		return end()
	default:
		return fmt.Errorf("incorrect syntax in: %v", query)
	}
}

func (t *Tester) runQuery(q data.Query) {
	if t.state.shouldSkip() {
		return
	}
	t.reporter.AddTestCase(q.Query, q.Line)
	parser := sqlparser.NewTestParser()
	ast, err := parser.Parse(q.Query)
	if err != nil {
		t.reporter.AddFailure(err)
		return
	}
	cfg := QueryRunConfig{
		ast:       ast,
		vitess:    !t.state.mysqlOnly,
		mysql:     !t.state.vitessOnly,
		reference: t.state.shouldReference(),
	}
	err = t.qr.runQuery(q, t.expectedErrs, cfg)
	if err != nil {
		t.reporter.AddFailure(err)
	}
	t.reporter.EndTestCase()
	// clear expected errors and current query after we execute any query
	t.expectedErrs = false
}

func (t *Tester) findTable(name string) (ks string, err error) {
	for ksName, ksSchema := range t.vschema.Keyspaces {
		for _, table := range ksSchema.Tables {
			if table.Name.String() == name {
				if ks != "" {
					return "", fmt.Errorf("table %s found in multiple keyspaces", name)
				}
				ks = ksName
			}
		}
	}
	if ks == "" {
		return "", fmt.Errorf("table %s not found in any keyspace", name)
	}
	return ks, nil
}

func (t *Tester) waitAuthoritative(query string) {
	var tblName, ksName string
	strs := strings.Split(query, " ")
	switch len(strs) {
	case 2:
		tblName = strs[1]
		var err error
		ksName, err = t.findTable(tblName)
		if err != nil {
			t.reporter.AddFailure(err)
			return
		}
	case 3:
		tblName = strs[1]
		ksName = strs[2]

	default:
		t.reporter.AddFailure(fmt.Errorf("expected table name and keyspace for wait_authoritative in: %v", query))
	}

	log.Infof("Waiting for authoritative schema for table %s", tblName)
	err := utils.WaitForAuthoritative(t.reporter, ksName, tblName, t.clusterInstance.VtgateProcess.ReadVSchema)
	if err != nil {
		t.reporter.AddFailure(fmt.Errorf("failed to wait for authoritative schema for table %s: %v", tblName, err))
	}
}

func newPrimaryKeyIndexDefinitionSingleColumn(name sqlparser.IdentifierCI) *sqlparser.IndexDefinition {
	index := &sqlparser.IndexDefinition{
		Info: &sqlparser.IndexInfo{
			Name: sqlparser.NewIdentifierCI("PRIMARY"),
			Type: sqlparser.IndexTypePrimary,
		},
		Columns: []*sqlparser.IndexColumn{{Column: name}},
	}
	return index
}

func (t *Tester) autoVSchema() bool {
	return t.vschemaFile == ""
}

func getShardingKeysForTable(create *sqlparser.CreateTable) (sks []sqlparser.IdentifierCI) {
	var allIdCI []sqlparser.IdentifierCI
	// first we normalize the primary keys
	for _, col := range create.TableSpec.Columns {
		if col.Type.Options.KeyOpt == sqlparser.ColKeyPrimary {
			create.TableSpec.Indexes = append(create.TableSpec.Indexes, newPrimaryKeyIndexDefinitionSingleColumn(col.Name))
			col.Type.Options.KeyOpt = sqlparser.ColKeyNone
		}
		allIdCI = append(allIdCI, col.Name)
	}

	// and now we can fetch the primary keys
	for _, index := range create.TableSpec.Indexes {
		if index.Info.Type == sqlparser.IndexTypePrimary {
			for _, column := range index.Columns {
				sks = append(sks, column.Column)
			}
		}
	}

	// if we have no primary keys, we'll use all columns as the sharding keys
	if len(sks) == 0 {
		sks = allIdCI
	}
	return
}

func (t *Tester) handleCreateTable(create *sqlparser.CreateTable) func() {
	sks := getShardingKeysForTable(create)

	shardingKeys := &vindexes.ColumnVindex{
		Columns: sks,
		Name:    "xxhash",
		Type:    "xxhash",
	}

	ks := t.vschema.Keyspaces[t.ksNames[0]]
	tableName := create.Table.Name
	ks.Tables[tableName.String()] = &vindexes.Table{
		Name:           tableName,
		Keyspace:       ks.Keyspace,
		ColumnVindexes: []*vindexes.ColumnVindex{shardingKeys},
	}

	ksJson, err := json.Marshal(ks)
	exitIf(err, "marshalling keyspace schema")

	err = t.clusterInstance.VtctldClientProcess.ApplyVSchema(t.ksNames[0], string(ksJson))
	exitIf(err, "applying vschema")

	return func() {
		err := utils.WaitForAuthoritative(t.reporter, t.ksNames[0], create.Table.Name.String(), t.clusterInstance.VtgateProcess.ReadVSchema)
		exitIf(err, "waiting for authoritative schema after auto-vschema update ")
	}
}
