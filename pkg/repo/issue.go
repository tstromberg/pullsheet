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

package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/go-github/v72/github"
	"k8s.io/klog/v2"

	"github.com/google/pullsheet/pkg/client"
	"github.com/google/pullsheet/pkg/ghcache"
)

// IssueSummary is a summary of a single PR
type IssueSummary struct {
	URL     string
	Date    string
	Author  string
	Closer  string
	Project string
	Type    string
	Title   string
}

// ClosedIssues returns a list of closed issues within a project
func ClosedIssues(ctx context.Context, c *client.Client, org string, project string, since time.Time, until time.Time, users []string) ([]*IssueSummary, error) {
	closed, err := issues(ctx, c, org, project, since, until, users, "closed")
	if err != nil {
		return nil, err
	}

	result := make([]*IssueSummary, 0, len(closed))
	for _, i := range closed {
		result = append(result, &IssueSummary{
			URL:     i.GetHTMLURL(),
			Date:    i.GetClosedAt().Format(dateForm),
			Author:  i.GetUser().GetLogin(),
			Closer:  i.GetClosedBy().GetLogin(),
			Project: project,
			Title:   i.GetTitle(),
		})
	}

	return result, nil
}

// fetchAllIssuePages fetches all pages of issues using the provided GitHub client and options,
// applying retry logic for each page request via ghcache.RetryGithubCall.
// It uses cursor-based pagination via ListCursorOptions.After and Response.NextPageToken.
func fetchAllIssuePages(
	ctx context.Context,
	ghClient *github.Client,
	org string,
	project string,
	listOpts *github.IssueListByRepoOptions, // IssueListByRepoOptions embeds ListOptions and ListCursorOptions
) ([]*github.Issue, error) {
	var allIssues []*github.Issue
	currentOpts := *listOpts // Make a copy to modify for pagination

	// Ensure old integer-based page pagination is disabled
	currentOpts.ListOptions.Page = 0
	// Ensure string-based page field in ListCursorOptions is disabled (we use 'After' or 'First')
	currentOpts.ListCursorOptions.Page = ""

	// Ensure PerPage is consistently set in ListCursorOptions.
	// If not set in ListCursorOptions initially, try ListOptions. If still not, default to 50.
	if currentOpts.ListCursorOptions.PerPage == 0 {
		if currentOpts.ListOptions.PerPage != 0 {
			currentOpts.ListCursorOptions.PerPage = currentOpts.ListOptions.PerPage
		} else {
			currentOpts.ListCursorOptions.PerPage = 100 // Default PerPage
		}
	}
	// Zero out ListOptions.PerPage as ListCursorOptions.PerPage is now the authority.
	currentOpts.ListOptions.PerPage = 0
	currentOpts.ListCursorOptions.First = 0 // Initialize First, will be set for the first call

	var currentAfterCursor string // This will hold the "after" cursor. Initially empty.

	for {
		if currentAfterCursor == "" { // This is the first effective fetch
			currentOpts.ListCursorOptions.First = currentOpts.ListCursorOptions.PerPage // Use 'First'
			currentOpts.ListCursorOptions.After = ""                                    // Ensure 'After' is empty
		} else { // Subsequent fetches
			currentOpts.ListCursorOptions.First = 0 // Do not use 'First'; rely on PerPage via URL params if needed, or API default.
			currentOpts.ListCursorOptions.After = currentAfterCursor
		}

		// Logging and Cache Key
		logDesc := ""
		if currentOpts.ListCursorOptions.After != "" {
			logDesc = fmt.Sprintf("after %s", currentOpts.ListCursorOptions.After)
		} else {
			logDesc = fmt.Sprintf("first %d", currentOpts.ListCursorOptions.First)
		}

		// Create a stable key for the request based on options and cursor logic
		stableOptsPart := fmt.Sprintf("state=%s-sort=%s-dir=%s-since=%s-labels=%s-assignee=%s-mentioned=%s-creator=%s-perpage=%d",
			listOpts.State, listOpts.Sort, listOpts.Direction, listOpts.Since.Format(time.RFC3339),
			strings.Join(listOpts.Labels, ","), listOpts.Assignee, listOpts.Mentioned, listOpts.Creator, currentOpts.ListCursorOptions.PerPage)
		// Differentiate cache key for initial "first N" request vs "after cursor" requests
		var cursorKeyPart string
		if currentOpts.ListCursorOptions.After != "" {
			cursorKeyPart = fmt.Sprintf("after-%s", currentOpts.ListCursorOptions.After)
		} else {
			cursorKeyPart = fmt.Sprintf("first-%d", currentOpts.ListCursorOptions.First)
		}
		cacheKey := fmt.Sprintf("list-issue-pages-%s-%s-%s-opts-%s", org, project, cursorKeyPart, stableOptsPart)

		callDesc := fmt.Sprintf("Issues.ListByRepo using %s for %s/%s (State: %s, Sort: %s, Direction: %s, PerPage: %d)",
			logDesc, org, project,
			listOpts.State, listOpts.Sort, listOpts.Direction, // Log initial overall request parameters
			currentOpts.ListCursorOptions.PerPage) // Log current PerPage being used

		apiCall := func() (interface{}, *github.Response, error) {
			// currentOpts is now prepared with either First (for initial) or After (for subsequent)
			pageIssuesData, ghRespData, errData := ghClient.Issues.ListByRepo(ctx, org, project, &currentOpts)
			return pageIssuesData, ghRespData, errData
		}

		klog.Info(callDesc)
		rawData, ghResp, err := ghcache.RetryGithubCall(ctx, cacheKey, callDesc, apiCall)
		if err != nil {
			return nil, err // Error is already formatted by RetryGithubCall
		}

		pageIssues, ok := rawData.([]*github.Issue)
		if !ok {
			// This can happen if rawData is nil (e.g. API call succeeded but returned no data, and apiCall returned nil for data part)
			// Or if the type assertion fails for other reasons.
			// If rawData is nil and there's no next page token, it's a valid end.
			if rawData == nil && (ghResp == nil || ghResp.After == "") {
				klog.V(1).Infof("No further issues found for %s (rawData is nil, no next page cursor in ghResp.After). Ending pagination.", callDesc)
				break
			}
			return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected []*github.Issue or nil)", callDesc, rawData)
		}

		if len(pageIssues) > 0 {
			allIssues = append(allIssues, pageIssues...)
		}

		// An API call can return 0 issues on a "page" but still have a next page cursor.

		if ghResp == nil || ghResp.After == "" { // Check ghResp.After for the next cursor
			klog.V(1).Infof("No next page cursor found in ghResp.After for %s (or ghResp is nil). Ending pagination.", callDesc)
			break
		}
		currentAfterCursor = ghResp.After // Prepare for the next iteration
	}

	klog.V(1).Infof("Fetched %d issues via paginated API calls for %s/%s before applying detailed filters.", len(allIssues), org, project)
	return allIssues, nil
}

// issues returns a list of issues in a project
func issues(ctx context.Context, c *client.Client, org string, project string, since time.Time, until time.Time, users []string, state string) ([]*github.Issue, error) {
	result := []*github.Issue{}
	opts := &github.IssueListByRepoOptions{
		State:     state,
		Sort:      "updated",
		Direction: "desc",
		ListOptions: github.ListOptions{
			PerPage: 50,
		},
	}

	matchUser := map[string]bool{}
	for _, u := range users {
		matchUser[strings.ToLower(u)] = true
	}

	klog.Infof("Gathering issues for %s/%s...", org, project)
	klog.V(1).Infof("...with initial filters: Users: %v, Since: %s, Until: %s, State: %s, Sort: %s, Direction: %s",
		users, since.Format(dateForm), until.Format(dateForm), opts.State, opts.Sort, opts.Direction)

	allFetchedIssues, err := fetchAllIssuePages(ctx, c.GitHubClient, org, project, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch issue list for %s/%s: %w", org, project, err)
	}

	if len(allFetchedIssues) == 0 {
		klog.Infof("No issues initially found for %s/%s (State: %s, Sort: %s, Direction: %s).", org, project, opts.State, opts.Sort, opts.Direction)
		return result, nil // result is empty
	}

	klog.Infof("Fetched %d issues for %s/%s via API. Now applying detailed local filters...", len(allFetchedIssues), org, project)
	klog.V(1).Infof("...Detailed filter criteria: Users: %v, Since: %s, Until: %s, State: %s",
		users, since.Format(dateForm), until.Format(dateForm), state)

	for _, i := range allFetchedIssues {
		if i.IsPullRequest() {
			klog.V(1).Infof("Skipping issue #%d (%q): It is a pull request.", i.GetNumber(), i.GetTitle())
			continue
		}

		// Filter 1: If this issue is already older than 'since', and issues are sorted by 'updated_at desc',
		// all subsequent issues will also be older. So, we can stop processing.
		if i.GetUpdatedAt().Before(since) {
			klog.V(1).Infof("Optimization: Issue #%d (%q, updated %s) is before 'since' date %s. As issues are sorted by updated_at desc, stopping further processing of fetched list.",
				i.GetNumber(), i.GetTitle(), i.GetUpdatedAt().Format(dateForm), since.Format(dateForm))
			break // Stop processing the rest of allFetchedIssues
		}

		// Filter 2: Issue must be closed (if state="closed"),
		// and its closed_at date must be within the [since, until] window.
		// For open issues, these specific closed_at checks are skipped.
		if state == "closed" {
			closedAt := i.GetClosedAt()
			if closedAt.IsZero() { // Should not happen if state is "closed" from API
				klog.Warningf("Issue #%d (%q) from API with state='closed' has zero ClosedAt. Skipping.", i.GetNumber(), i.GetTitle())
				continue
			}
			if closedAt.After(until) {
				klog.V(1).Infof("Skipping issue #%d (%q): Closed at %s (after 'until' %s)", i.GetNumber(), i.GetTitle(), closedAt.Format(dateForm), until.Format(dateForm))
				continue
			}
			if closedAt.Before(since) {
				klog.V(1).Infof("Skipping issue #%d (%q): Closed at %s (before 'since' %s)", i.GetNumber(), i.GetTitle(), closedAt.Format(dateForm), since.Format(dateForm))
				continue
			}
		}

		// Filter 3: State filter (redundant if opts.State worked, but safe)
		if state != "" && i.GetState() != state {
			klog.V(1).Infof("Skipping issue #%d (%q): State is %q, not desired %q.", i.GetNumber(), i.GetTitle(), i.GetState(), state)
			continue
		}

		// At this point, the issue from the list passes initial filters. Now fetch its full details for user matching.
		t := issueDate(i) // Determine the timestamp for cache interaction
		klog.V(1).Infof("Fetching full issue details for #%d (%q, closed %s, updated %s)", i.GetNumber(), i.GetTitle(), i.GetClosedAt().Format(dateForm), i.GetUpdatedAt().Format(dateForm))

		full, err := ghcache.IssuesGet(ctx, c.Cache, c.GitHubClient, t, org, project, i.GetNumber())
		if err != nil {
			klog.Warningf("Failed to get full issue details for #%d (%s/%s): %v. Skipping this issue.", i.GetNumber(), org, project, err)
			continue
		}

		// Filter 4: User filter (applied to the full issue object)
		// Match if either creator or closer (if applicable) is in the user list.
		// For open issues, closer might be nil or empty.
		creatorLogin := full.GetUser().GetLogin()
		matchesUserFilter := false
		if len(matchUser) == 0 {
			matchesUserFilter = true // No user filter means all users match
		} else {
			if matchUser[strings.ToLower(creatorLogin)] {
				matchesUserFilter = true
			}
			if full.GetClosedBy() != nil { // ClosedBy can be nil for open issues
				closerLogin := full.GetClosedBy().GetLogin()
				if closerLogin != "" && matchUser[strings.ToLower(closerLogin)] {
					matchesUserFilter = true
				}
			}
		}

		if !matchesUserFilter {
			klog.V(1).Infof("Skipping issue #%d (%q) by %s (closer: %s): Neither creator nor closer in specified user list.",
				full.GetNumber(), full.GetTitle(), creatorLogin, full.GetClosedBy().GetLogin())
			continue
		}

		klog.Infof("Including issue #%d (%q): State %q, Created by %s, Closed by %s at %s.",
			full.GetNumber(), full.GetTitle(), full.GetState(), creatorLogin, full.GetClosedBy().GetLogin(), full.GetClosedAt().Format(dateForm))
		result = append(result, full)
	}

	klog.Infof("Returning %d issues after all filters.", len(result))
	return result, nil
}

func issueDate(i *github.Issue) time.Time {
	t := i.GetClosedAt()
	if t.IsZero() {
		t = i.GetUpdatedAt()
	}
	if t.IsZero() {
		t = i.GetCreatedAt()
	}

	return t.Time
}
