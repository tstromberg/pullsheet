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
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v33/github"
	"k8s.io/klog/v2"

	"github.com/google/pullsheet/pkg/client"
	"github.com/google/pullsheet/pkg/ghcache"
)

const dateForm = "2006-01-02"

var (
	ignorePathRe = regexp.MustCompile(`go\.mod|go\.sum|vendor/|third_party|ignore|schemas/v\d|schema/v\d|Gopkg.lock|.DS_Store|\.json$|\.pb\.go|references/api/grpc|docs/commands/|pb\.gw\.go|proto/.*\.tmpl|proto/.*\.md`)
	truncRe      = regexp.MustCompile(`changelog|CHANGELOG|Gopkg.toml`)
	commentRe    = regexp.MustCompile(`<!--.*?>`)
)

// fetchAllPullRequestPages fetches all pages of pull requests using the provided GitHub client and options,
// applying retry logic for each page request via ghcache.RetryGithubCall.
func fetchAllPullRequestPages(
	ctx context.Context,
	ghClient *github.Client,
	org string,
	project string,
	listOpts *github.PullRequestListOptions,
) ([]*github.PullRequest, error) {
	var allPRs []*github.PullRequest
	currentOpts := *listOpts // Make a copy to modify Page

	// Ensure we start at the first page if not already set.
	// GitHub defaults to page 1 if opts.Page is 0.
	// The loop structure relies on currentOpts.Page being explicitly managed.
	if currentOpts.Page == 0 {
		currentOpts.Page = 1
	}

	for {
		// For RetryGithubCall, the key is primarily for logging context.
		key := fmt.Sprintf("list-pr-pages-%s-%s-page%d", org, project, currentOpts.Page)
		callDesc := fmt.Sprintf("PullRequests.List page %d for %s/%s (State: %s, Sort: %s, Direction: %s)", currentOpts.Page, org, project, listOpts.State, listOpts.Sort, listOpts.Direction)
		klog.Info(callDesc)

		apiCall := func() (interface{}, *github.Response, error) {
			// This function is passed to RetryGithubCall, so it needs to match the expected signature.
			// It captures ghClient, org, project, and currentOpts from the outer scope.
			pagePRsData, ghRespData, errData := ghClient.PullRequests.List(ctx, org, project, &currentOpts)
			return pagePRsData, ghRespData, errData
		}

		rawData, ghResp, err := ghcache.RetryGithubCall(ctx, key, callDesc, apiCall)
		if err != nil {
			return nil, err // Error is already formatted by RetryGithubCall
		}

		pagePRs, ok := rawData.([]*github.PullRequest)
		if !ok {
			return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected []*github.PullRequest)", callDesc, rawData)
		}

		if len(pagePRs) == 0 { // No more PRs on this page.
			klog.V(1).Infof("No pull requests found on page %d for %s/%s with specified criteria. Ending pagination.", currentOpts.Page, org, project)
			break
		}

		allPRs = append(allPRs, pagePRs...)

		if ghResp.NextPage == 0 {
			break
		}
		currentOpts.Page = ghResp.NextPage
	}
	klog.V(1).Infof("Fetched %d pull requests via paginated API calls for %s/%s before applying detailed filters.", len(allPRs), org, project)
	return allPRs, nil
}

// MergedPulls returns a list of pull requests in a project
func MergedPulls(ctx context.Context, c *client.Client, org string, project string, since time.Time, until time.Time, users []string, branches []string) ([]*github.PullRequest, error) {
	var result []*github.PullRequest

	opts := &github.PullRequestListOptions{
		State:     "closed",
		Sort:      "updated",
		Direction: "desc",
		ListOptions: github.ListOptions{
			PerPage: 100,
		},
	}

	matchUser := map[string]bool{}
	for _, u := range users {
		matchUser[strings.ToLower(u)] = true
	}

	matchBranch := map[string]bool{}
	for _, b := range branches {
		matchBranch[strings.ToLower(b)] = true
	}

	klog.Infof("Gathering pull requests for %s/%s...", org, project)
	klog.V(1).Infof("...with initial filters: Users: %v, Branches: %v, Since: %s, Until: %s, State: %s, Sort: %s, Direction: %s",
		users, branches, since.Format(dateForm), until.Format(dateForm), opts.State, opts.Sort, opts.Direction)

	// opts.Page will be managed by fetchAllPullRequestPages.
	// It's initialized to 0 in opts, which fetchAllPullRequestPages handles as page 1.
	allFetchedPRs, err := fetchAllPullRequestPages(ctx, c.GitHubClient, org, project, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch pull request list for %s/%s: %w", org, project, err)
	}

	if len(allFetchedPRs) == 0 {
		klog.Infof("No pull requests initially found for %s/%s (State: %s, Sort: %s, Direction: %s).", org, project, opts.State, opts.Sort, opts.Direction)
		return result, nil // result is empty
	}

	klog.Infof("Fetched %d pull requests for %s/%s via API. Now applying detailed local filters...", len(allFetchedPRs), org, project)
	klog.V(1).Infof("...Detailed filter criteria: Users: %v, Branches: %v, Since: %s, Until: %s",
		users, branches, since.Format(dateForm), until.Format(dateForm))

	for _, pr := range allFetchedPRs {
		// Filter 1: If this PR is already older than 'since', and PRs are sorted by 'updated_at desc',
		// all subsequent PRs will also be older. So, we can stop processing.
		if pr.GetUpdatedAt().Before(since) {
			klog.V(1).Infof("Optimization: PR #%d ('%s', updated %s) is before 'since' date %s. As PRs are sorted by updated_at desc, stopping further processing of fetched list.",
				pr.GetNumber(), pr.GetTitle(), pr.GetUpdatedAt().Format(dateForm), since.Format(dateForm))
			break // Stop processing the rest of allFetchedPRs
		}

		// Filter 2: PR must be closed (this is already part of `opts.State`),
		// and its closed_at date must be within the [since, until] window.
		closedAt := pr.GetClosedAt()
		if closedAt.IsZero() { // Not closed, or data missing
			klog.V(1).Infof("Skipping PR #%d ('%s'): Not closed (ClosedAt is zero). State from API: %s", pr.GetNumber(), pr.GetTitle(), pr.GetState())
			continue
		}
		if closedAt.After(until) {
			klog.V(1).Infof("Skipping PR #%d ('%s'): Closed at %s (after 'until' %s)", pr.GetNumber(), pr.GetTitle(), closedAt.Format(dateForm), until.Format(dateForm))
			continue
		}
		if closedAt.Before(since) {
			klog.V(1).Infof("Skipping PR #%d ('%s'): Closed at %s (before 'since' %s)", pr.GetNumber(), pr.GetTitle(), closedAt.Format(dateForm), since.Format(dateForm))
			continue
		}

		// Filter 3: User filter
		prUserLogin := pr.GetUser().GetLogin()
		uname := strings.ToLower(prUserLogin)
		if len(matchUser) > 0 && !matchUser[uname] {
			klog.V(1).Infof("Skipping PR #%d by %s: User not in specified list.", pr.GetNumber(), prUserLogin)
			continue
		}

		// Filter 4: Bot filter
		if isBot(pr.GetUser()) {
			klog.V(1).Infof("Skipping PR #%d by %s: Detected as bot.", pr.GetNumber(), prUserLogin)
			continue
		}

		// Filter 5: Ensure PR is actually in "closed" state (redundant if opts.State="closed" worked, but safe)
		if pr.GetState() != "closed" {
			klog.V(1).Infof("Skipping PR #%d by %s: State is '%s', not 'closed'.", pr.GetNumber(), prUserLogin, pr.GetState())
			continue
		}

		// At this point, the PR from the list passes initial filters. Now fetch its full details.
		klog.V(1).Infof("Fetching full PR details for #%d by %s (updated %s): '%s'", pr.GetNumber(), prUserLogin, pr.GetUpdatedAt().Format(dateForm), pr.GetTitle())

		cacheTime := pr.GetMergedAt()
		if cacheTime.IsZero() {
			cacheTime = pr.GetClosedAt()
		}
		if cacheTime.IsZero() { // Should be rare if ClosedAt was set.
			cacheTime = pr.GetUpdatedAt()
			klog.Warningf("PR #%d ('%s') had zero MergedAt and ClosedAt from list; using UpdatedAt for cache key time.", pr.GetNumber(), pr.GetTitle())
		}

		fullPR, err := ghcache.PullRequestsGet(ctx, c.Cache, c.GitHubClient, cacheTime, org, project, pr.GetNumber())
		if err != nil {
			// This error is already logged by ghcache.RetryGithubCall if it's from the API call after retries.
			// Add context specific to this loop.
			klog.Warningf("Failed to get full PR details for #%d (%s/%s): %v. Skipping this PR.", pr.GetNumber(), org, project, err)
			continue
		}

		// Filter 6: Branch filter (applied to the fullPR object)
		branch := fullPR.GetBase().GetRef()
		if len(matchBranch) > 0 && !matchBranch[branch] {
			klog.V(1).Infof("Skipping PR #%d ('%s'): Merged to branch '%s', which is not in specified list %v.", fullPR.GetNumber(), fullPR.GetTitle(), branch, branches)
			continue
		}

		// Filter 7: Must be merged (applied to the fullPR object)
		if !fullPR.GetMerged() || fullPR.GetMergeCommitSHA() == "" {
			klog.V(1).Infof("Skipping PR #%d ('%s'): Not merged or merge commit SHA is missing.", fullPR.GetNumber(), fullPR.GetTitle())
			continue
		}

		// Filter 8: MergedAt timestamp must be within [since, until] (applied to fullPR object)
		mergedAt := fullPR.GetMergedAt()
		if mergedAt.IsZero() { // This should ideally not happen if fullPR.GetMerged() is true.
			klog.Warningf("PR #%d ('%s') was marked as merged but MergedAt timestamp is zero. Skipping.", fullPR.GetNumber(), fullPR.GetTitle())
			continue
		}
		if mergedAt.After(until) {
			klog.V(1).Infof("Skipping PR #%d ('%s'): Merged at %s (after 'until' %s)", fullPR.GetNumber(), fullPR.GetTitle(), mergedAt.Format(dateForm), until.Format(dateForm))
			continue
		}
		if mergedAt.Before(since) {
			klog.V(1).Infof("Skipping PR #%d ('%s'): Merged at %s (before 'since' %s)", fullPR.GetNumber(), fullPR.GetTitle(), mergedAt.Format(dateForm), since.Format(dateForm))
			continue
		}

		klog.Infof("Including PR #%d ('%s'): Merged by %s to branch '%s' at %s.", fullPR.GetNumber(), fullPR.GetTitle(), fullPR.GetUser().GetLogin(), branch, mergedAt.Format(dateForm))
		result = append(result, fullPR)
	}
	klog.Infof("Returning %d pull request results after all filters.", len(result))
	return result, nil
}

// PRSummary is a summary of a single PR
type PRSummary struct {
	URL         string
	Date        string
	User        string
	Project     string
	Type        string
	Title       string
	Delta       int
	Added       int
	Deleted     int
	FilesTotal  int
	Files       string // newline delimited
	Description string
}

// PullSummary converts GitHub PR data into a summarized view
func PullSummary(prs map[*github.PullRequest][]github.CommitFile, since time.Time, until time.Time) ([]*PRSummary, error) {
	sum := []*PRSummary{}
	seen := map[string]bool{}

	for pr, files := range prs {
		if seen[pr.GetHTMLURL()] {
			klog.Infof("skipping seen issue: %s", pr.GetHTMLURL())
			continue
		}
		seen[pr.GetHTMLURL()] = true

		_, project := ParseURL(pr.GetHTMLURL())
		body := pr.GetBody()
		body = commentRe.ReplaceAllString(body, "")

		if len(body) > 240 {
			body = body[0:240] + "..."
		}
		t := pr.GetMergedAt()
		// Often the merge timestamp is empty :(
		if t.IsZero() {
			t = pr.GetClosedAt()
		}

		if t.After(until) {
			klog.Infof("skipping %s - closed at %s, after %s", pr.GetHTMLURL(), t, until)
			continue
		}

		if t.Before(since) {
			klog.Infof("skipping %s - closed at %s, before %s", pr.GetHTMLURL(), t, since)
			continue
		}

		added := 0
		paths := []string{}
		deleted := 0

		for _, f := range files {
			// These files are mostly auto-generated
			if truncRe.MatchString(f.GetFilename()) && f.GetAdditions() > 10 {
				klog.Infof("truncating %s from %d to %d lines added", f.GetFilename(), f.GetAdditions(), 10)
				added += 10
			} else {
				klog.Infof("%s - %d added, %d deleted", f.GetFilename(), f.GetAdditions(), f.GetDeletions())
				added += f.GetAdditions()
			}
			deleted += f.GetDeletions()
			paths = append(paths, f.GetFilename())
		}
		klog.Infof("%s had %d files to consider - %d added, %d deleted", pr.GetHTMLURL(), len(files), added, deleted)

		sum = append(sum, &PRSummary{
			URL:         pr.GetHTMLURL(),
			Date:        t.Format(dateForm),
			Project:     project,
			Type:        prType(files),
			Title:       pr.GetTitle(),
			User:        pr.GetUser().GetLogin(),
			Delta:       added + deleted,
			Added:       added,
			Deleted:     deleted,
			FilesTotal:  pr.GetChangedFiles(),
			Files:       strings.Join(paths, "\n"),
			Description: body,
		})
	}

	return sum, nil
}
