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

package cmd

import (
	"github.com/spf13/cobra"

	"github.com/vitessio/vt/go/summarize"
)

func summarizeCmd(port *int64) *cobra.Command {
	cfg := summarize.Config{
		WState: wstate,
	}

	cmd := &cobra.Command{
		Use:     "summarize old_file.json [new_file.json]",
		Aliases: []string{"benchstat"},
		Short:   "Compares and analyses a trace output",
		Example: "vt summarize old.json new.json",
		Args:    cobra.RangeArgs(1, 2),
		Run: func(_ *cobra.Command, args []string) {
			cfg.Files = args
			cfg.Port = *port
			summarize.Run(&cfg)
		},
	}

	cmd.Flags().StringVar(&cfg.HotMetric, "hot-metric", "total-time", "Metric to determine hot queries (options: usage-count, total-rows-examined, avg-rows-examined, avg-time, total-time)")
	cmd.Flags().BoolVar(&cfg.ShowGraph, "graph", false, "Show the query graph in the browser")
	cmd.Flags().StringVar(&cfg.OutputFormat, "format", "html", "Output format (options: html, markdown)")

	return cmd
}
