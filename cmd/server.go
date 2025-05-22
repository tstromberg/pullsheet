// Copyright 2021 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/google/pullsheet/pkg/client"
	"github.com/google/pullsheet/pkg/server"
	"github.com/google/pullsheet/pkg/server/job"
)

// serverCmd represents the subcommand for `pullsheet server`
var serverCmd = &cobra.Command{
	Use:           "server",
	Short:         "Serve leaderboard data with web UI",
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServer(rootOpts)
	},
}

var port int

func init() {
	serverCmd.Flags().IntVar(
		&port,
		"port",
		8080,
		"Port for server to listen on")
	serverCmd.Flags().BoolVar(
		&disableCaching,
		"no-caching",
		false,
		"Disable caching on resulting HTML files")

	rootCmd.AddCommand(serverCmd)
}

func runServer(rootOpts *rootOptions) error {
	ctx := context.Background()
	c, err := client.New(ctx, client.Config{GitHubTokenPath: rootOpts.tokenPath})
	if err != nil {
		return err
	}

	// setup initial job
	j := job.New(
		&job.Opts{
			Repos:          rootOpts.repos,
			Users:          rootOpts.users,
			Branches:       rootOpts.branches,
			Since:          rootOpts.sinceParsed,
			Until:          rootOpts.untilParsed,
			Title:          rootOpts.title,
			DisableCaching: disableCaching,
		})

	s := server.New(ctx, c, j)
	http.HandleFunc("/", s.Root())
	http.HandleFunc("/home", s.Home())
	http.HandleFunc("/job/", s.Job())
	http.HandleFunc("/new-job", s.NewJob())
	http.HandleFunc("/healthz", s.Healthz())
	http.HandleFunc("/threadz", s.Threadz())

	listenAddr := fmt.Sprintf(":%s", os.Getenv("PORT"))
	if listenAddr == ":" {
		listenAddr = fmt.Sprintf(":%d", port)
	}
	return http.ListenAndServe(listenAddr, nil)
}
