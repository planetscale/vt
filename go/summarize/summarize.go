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

package summarize

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/chroma/quick"
	"golang.org/x/term"

	"github.com/vitessio/vt/go/keys"
)

type (
	readingSummary struct {
		Name string

		// Only one of these fields will be populated
		TracedQueries   []TracedQuery // Set when analyzing a 'vt tester --trace' output
		AnalysedQueries *keys.Output  // Set when analyzing a 'vt keys' output
	}

	fileInfo struct {
		filename string
		fileType
	}
)

func Run(files []string, hotMetric string) {
	var dbInfoPath string
	var filesToRead []fileInfo
	var hasTrace bool

	// if we have tracefiles, handle them
	// otherwise, create a summary and feed it to all json inputs
	// move rendering to this spot
	// todo: add file types for other json types. Right now just checks for dbinfo files, else defaults

	for _, file := range files {
		typ, _ := getFileType(file)
		switch typ {
		case dbInfoFile:
			fmt.Printf("dbinfo file: %s\n", file)
			dbInfoPath = file
		case transactionFile:
			fmt.Printf("transaction file: %s\n", file)
		case traceFile:
			filesToRead = append(filesToRead, fileInfo{filename: file, fileType: traceFile})
			hasTrace = true
		case keysFile:
			filesToRead = append(filesToRead, fileInfo{filename: file, fileType: keysFile})
		default:
			panic("Unknown file type")
		}
	}
	checkTraceConditions(hasTrace, filesToRead, hotMetric)

	rs := make([]readingSummary, len(filesToRead))
	var err error
	for i, f := range filesToRead {
		rs[i], err = readTraceFile(f)
		if err != nil {
			exit(err.Error())
		}
	}

	if hasTrace {
		if len(rs) == 2 {
			compareTraces(os.Stdout, terminalWidth(), highlightQuery, rs[0], rs[1])
		} else {
			printTraceSummary(os.Stdout, terminalWidth(), highlightQuery, rs[0])
		}
		return
	}

	printKeysSummary(os.Stdout, rs[0].Name, rs[0].AnalysedQueries, time.Now(), hotMetric, dbInfoPath)
}

func checkTraceConditions(hasTrace bool, filesToRead []fileInfo, hotMetric string) {
	if !hasTrace {
		return
	}
	for _, f := range filesToRead {
		if f.fileType != traceFile {
			panic("Trace files cannot be mixed with other file types")
		}
	}
	if len(filesToRead) > 2 {
		panic("Can only summarize up to two trace files at once")
	}
	if hotMetric != "" {
		exit("hotMetric flag is only supported for 'vt keys' output")
	}
}

func exit(msg string) {
	fmt.Println(msg)
	os.Exit(1)
}

const queryPrefix = "Query: "

func limitQueryLength(query string, termWidth int) string {
	// Process the query string
	processedQuery := strings.ReplaceAll(query, "\n", " ") // Replace newlines with spaces
	processedQuery = strings.TrimSpace(processedQuery)     // Trim leading/trailing spaces

	// Calculate available space for query
	availableSpace := termWidth - len(queryPrefix) - 3 // 3 for ellipsis

	if len(processedQuery) > availableSpace {
		processedQuery = processedQuery[:availableSpace] + "..."
	}
	return processedQuery
}

type Highlighter func(out io.Writer, query string) error

func highlightQuery(out io.Writer, query string) error {
	return quick.Highlight(out, query, "sql", "terminal", "monokai")
}

func noHighlight(out io.Writer, query string) error {
	_, err := fmt.Fprint(out, query)
	return err
}

func printQuery(out io.Writer, terminalWidth int, highLighter Highlighter, q TracedQuery, significant bool) {
	fmt.Fprintf(out, "%s", queryPrefix)
	err := highLighter(out, limitQueryLength(q.Query, terminalWidth))
	if err != nil {
		return
	}
	improved := ""
	if significant {
		improved = " (significant)"
	}
	fmt.Fprintf(out, "\nLine # %s%s\n", q.LineNumber, improved)
}

func terminalWidth() int {
	termWidth, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 80 // default to 80 if we can't get the terminal width
	}
	return termWidth
}
