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
	"github.com/fatih/color"
	"io"
	"net/http"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"vitess.io/vitess/go/test/endtoend/cluster"
)

type Config struct {
	LogLevel             string
	OLAP                 bool
	Sharded              bool
	XUnit                bool
	VschemaFile          string
	VtExplainVschemaFile string
	TraceFile            string
	Tests                []string
}

func Run(cfg Config) {
	err := CheckEnvironment()
	if err != nil {
		exitIf(err, "reading environment variables")
	}

	a := cfg.VschemaFile != ""
	b := cfg.VtExplainVschemaFile != ""
	if a && b || a && cfg.Sharded || b && cfg.Sharded {
		log.Errorf("specify only one of the following flags: -vschema, -vtexplain-vschema, -sharded")
		os.Exit(1)
	}

	if ll := os.Getenv("LOG_LEVEL"); ll != "" {
		cfg.LogLevel = ll
	}
	if cfg.LogLevel != "" {
		ll, err := log.ParseLevel(cfg.LogLevel)
		if err != nil {
			log.Errorf("error parsing log level %s: %v", cfg.LogLevel, err)
		}
		log.SetLevel(ll)
	}

	if len(cfg.Tests) == 0 {
		log.Errorf("no tests specified")
		os.Exit(1)
	}

	log.Infof("running tests: %v", cfg.Tests)

	clusterInstance, vtParams, mysqlParams, ksNames, closer := SetupCluster(cfg.VschemaFile, cfg.VtExplainVschemaFile, cfg.Sharded)
	defer closer()

	// remove errors folder if exists
	err = os.RemoveAll("errors")
	exitIf(err, "removing errors folder")

	var reporterSuite Suite
	if cfg.XUnit {
		reporterSuite = NewXMLTestSuite()
	} else {
		reporterSuite = NewFileReporterSuite(getVschema(clusterInstance))
	}
	failed := ExecuteTests(clusterInstance, vtParams, mysqlParams, cfg.Tests, reporterSuite, ksNames, cfg.VschemaFile, cfg.VtExplainVschemaFile, cfg.OLAP, getQueryRunnerFactory(cfg.TraceFile))
	outputFile := reporterSuite.Close()
	if failed {
		log.Errorf("some tests failed 😭\nsee errors in %v", outputFile)
		os.Exit(1)
	}
	println("Great, All tests passed")
}

func getQueryRunnerFactory(traceFile string) QueryRunnerFactory {
	inner := ComparingQueryRunnerFactory{}
	if traceFile == "" {
		return inner
	}

	var err error
	writer, err := os.Create(traceFile)
	exitIf(err, "creating trace file")

	_, err = writer.Write([]byte("["))
	exitIf(err, "writing to trace file")
	return NewTracerFactory(writer, inner)
}

func getVschema(clusterInstance *cluster.LocalProcessCluster) func() []byte {
	return func() []byte {
		httpClient := &http.Client{Timeout: 5 * time.Second}
		resp, err := httpClient.Get(clusterInstance.VtgateProcess.VSchemaURL)
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

func exitIf(err error, message string) {
	if err == nil {
		return
	}
	c := color.New(color.FgRed)
	_, _ = c.Fprintf(os.Stderr, "%s: %s\n", message, err.Error())
	os.Exit(1)
}
