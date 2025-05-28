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

package ghcache

import (
	"context"
	"fmt"
	"time"

	"github.com/avast/retry-go"
	"github.com/google/go-github/v33/github"
	"github.com/google/triage-party/pkg/persist"
	"k8s.io/klog/v2"
)

// RetryGithubCall performs a GitHub API call with retry logic.
// key is the cache key, used for logging context.
// callDesc is a descriptive string for the API call, used in logs and errors.
// apiCallFunc is the function that makes the actual GitHub API call.
func RetryGithubCall(
	ctx context.Context,
	key string,
	callDesc string,
	apiCallFunc func() (interface{}, *github.Response, error),
) (interface{}, *github.Response, error) {
	var data interface{}
	var ghResp *github.Response
	var opError error

	retryErr := retry.Do(
		func() error {
			data, ghResp, opError = apiCallFunc()
			return opError
		},
		retry.Context(ctx),
		retry.OnRetry(func(n uint, e error) {
			klog.Warningf("Retry #%d for cache key %s (%s) due to: %v", n, key, callDesc, e)
		}),
		retry.DelayType(retry.BackOffDelay),
		retry.MaxJitter(250*time.Millisecond),
	)

	if retryErr != nil {
		return nil, nil, fmt.Errorf("%s after retries: %w", callDesc, retryErr)
	}
	return data, ghResp, nil
}

// PullRequestsGet gets a pull request data from the cache or GitHub.
func PullRequestsGet(ctx context.Context, p persist.Cacher, c *github.Client, t time.Time, org string, project string, num int) (*github.PullRequest, error) {
	key := fmt.Sprintf("pr-%s-%s-%d", org, project, num)
	val := p.Get(key, t)

	if val != nil {
		klog.Infof("cache hit: %v", key)
		return val.GHPullRequest, nil
	}

	klog.Infof("cache miss for %v", key)
	callDesc := fmt.Sprintf("PullRequests.Get %s/%s#%d", org, project, num)
	apiCall := func() (interface{}, *github.Response, error) {
		// The response object contains rate limit info, etc., but is not directly used by this function's caller.
		// It's passed through retryGithubCall in case it's needed by other callers of the helper,
		// and for consistency in the helper's signature.
		return c.PullRequests.Get(ctx, org, project, num)
	}

	rawData, _, err := RetryGithubCall(ctx, key, callDesc, apiCall)
	if err != nil {
		return nil, err // Error is already formatted by RetryGithubCall
	}

	pr, ok := rawData.(*github.PullRequest)
	if !ok {
		return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected *github.PullRequest)", callDesc, rawData)
	}
	return pr, p.Set(key, &persist.Blob{GHPullRequest: pr})
}

// PullRequestsListFiles gets a list of files in a pull request from the cache or GitHub.
func PullRequestsListFiles(ctx context.Context, p persist.Cacher, c *github.Client, t time.Time, org string, project string, num int) ([]*github.CommitFile, error) {
	key := fmt.Sprintf("pr-listfiles-%s-%s-%d", org, project, num)
	val := p.Get(key, t)

	if val != nil {
		return val.GHCommitFiles, nil
	}

	klog.Infof("cache miss for %v", key)

	opts := &github.ListOptions{PerPage: 100}
	fs := []*github.CommitFile{}

	for {
		callDesc := fmt.Sprintf("PullRequests.ListFiles page %d for %s/%s#%d", opts.Page, org, project, num)
		apiCall := func() (interface{}, *github.Response, error) {
			pageFiles, ghResp, err := c.PullRequests.ListFiles(ctx, org, project, num, opts)
			return pageFiles, ghResp, err
		}

		rawData, ghResp, err := RetryGithubCall(ctx, key, callDesc, apiCall)
		if err != nil {
			return nil, err
		}

		pageFiles, ok := rawData.([]*github.CommitFile)
		if !ok {
			return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected []*github.CommitFile)", callDesc, rawData)
		}

		fs = append(fs, pageFiles...)

		if ghResp.NextPage == 0 {
			break
		}
		opts.Page = ghResp.NextPage
	}

	return fs, p.Set(key, &persist.Blob{GHCommitFiles: fs})
}

// PullRequestsListComments gets a list of comments in a pull request from the cache or GitHub for a given org, project, and number.
func PullRequestsListComments(ctx context.Context, p persist.Cacher, c *github.Client, t time.Time, org string, project string, num int) ([]*github.PullRequestComment, error) {
	key := fmt.Sprintf("pr-comments-%s-%s-%d", org, project, num)
	val := p.Get(key, t)

	if val != nil {
		return val.GHPullRequestComments, nil
	}

	klog.Infof("cache miss for %v", key)

	cs := []*github.PullRequestComment{}
	opts := &github.PullRequestListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		callDesc := fmt.Sprintf("PullRequests.ListComments page %d for %s/%s#%d", opts.ListOptions.Page, org, project, num)
		apiCall := func() (interface{}, *github.Response, error) {
			pageComments, ghResp, err := c.PullRequests.ListComments(ctx, org, project, num, opts)
			return pageComments, ghResp, err
		}

		rawData, ghResp, err := RetryGithubCall(ctx, key, callDesc, apiCall)
		if err != nil {
			return nil, err
		}

		pageComments, ok := rawData.([]*github.PullRequestComment)
		if !ok {
			return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected []*github.PullRequestComment)", callDesc, rawData)
		}
		cs = append(cs, pageComments...)

		if ghResp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = ghResp.NextPage
	}

	return cs, p.Set(key, &persist.Blob{GHPullRequestComments: cs})
}

// IssuesGet gets an issue from the cache or GitHub for a given org, project, and number.
func IssuesGet(ctx context.Context, p persist.Cacher, c *github.Client, t time.Time, org string, project string, num int) (*github.Issue, error) {
	key := fmt.Sprintf("issue-%s-%s-%d", org, project, num)
	val := p.Get(key, t)

	if val != nil {
		return val.GHIssue, nil
	}

	klog.Infof("cache miss for %v", key)

	callDesc := fmt.Sprintf("Issues.Get %s/%s#%d", org, project, num)
	apiCall := func() (interface{}, *github.Response, error) {
		return c.Issues.Get(ctx, org, project, num)
	}

	rawData, _, err := RetryGithubCall(ctx, key, callDesc, apiCall)
	if err != nil {
		return nil, err
	}

	issue, ok := rawData.(*github.Issue)
	if !ok {
		return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected *github.Issue)", callDesc, rawData)
	}
	return issue, p.Set(key, &persist.Blob{GHIssue: issue})
}

// IssuesListComments gets a list of comments in an issue from the cache or GitHub for a given org, project, and number.
func IssuesListComments(ctx context.Context, p persist.Cacher, c *github.Client, t time.Time, org string, project string, num int) ([]*github.IssueComment, error) {
	key := fmt.Sprintf("issue-comments-%s-%s-%d", org, project, num)
	val := p.Get(key, t)

	if val != nil {
		return val.GHIssueComments, nil
	}

	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	cs := []*github.IssueComment{}
	for {
		callDesc := fmt.Sprintf("Issues.ListComments page %d for %s/%s#%d", opts.ListOptions.Page, org, project, num)
		apiCall := func() (interface{}, *github.Response, error) {
			pageComments, ghResp, err := c.Issues.ListComments(ctx, org, project, num, opts)
			return pageComments, ghResp, err
		}

		rawData, ghResp, err := RetryGithubCall(ctx, key, callDesc, apiCall)
		if err != nil {
			return nil, err
		}

		pageComments, ok := rawData.([]*github.IssueComment)
		if !ok {
			return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected []*github.IssueComment)", callDesc, rawData)
		}
		cs = append(cs, pageComments...)

		if ghResp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = ghResp.NextPage
	}

	return cs, p.Set(key, &persist.Blob{GHIssueComments: cs})
}

func Reviews(ctx context.Context, p persist.Cacher, c *github.Client, t time.Time, org string, project string, num int) ([]*github.PullRequestReview, error) {
	key := fmt.Sprintf("review-%s-%s-%d", org, project, num)
	val := p.Get(key, t)

	if val != nil {
		return val.GHReviews, nil
	}

	opts := &github.ListOptions{PerPage: 100}
	rs := []*github.PullRequestReview{}
	for {
		callDesc := fmt.Sprintf("PullRequests.ListReviews page %d for %s/%s#%d", opts.Page, org, project, num)
		apiCall := func() (interface{}, *github.Response, error) {
			pageReviews, ghResp, err := c.PullRequests.ListReviews(ctx, org, project, num, opts)
			return pageReviews, ghResp, err
		}

		rawData, ghResp, err := RetryGithubCall(ctx, key, callDesc, apiCall)
		if err != nil {
			return nil, err
		}

		pageReviews, ok := rawData.([]*github.PullRequestReview)
		if !ok {
			return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected []*github.PullRequestReview)", callDesc, rawData)
		}
		rs = append(rs, pageReviews...)

		if ghResp.NextPage == 0 {
			break
		}
		opts.Page = ghResp.NextPage
	}

	return rs, p.Set(key, &persist.Blob{GHReviews: rs})
}

// FetchPullRequestsListPagesWithRetries fetches all pages of pull requests using the provided GitHub client and options,
// applying retry logic for each page request. It does not cache the list results directly but ensures
// the API calls are retried.
func FetchPullRequestsListPagesWithRetries(
	ctx context.Context,
	ghClient *github.Client,
	org string,
	project string,
	listOpts *github.PullRequestListOptions, // Use the original listOpts to maintain PerPage and other settings
) ([]*github.PullRequest, error) {
	var allPRs []*github.PullRequest
	currentOpts := *listOpts // Make a copy to modify Page
	currentOpts.Page = 0     // Ensure we start at the first page if not already set (or rely on GitHub's default if 0)

	for {
		// For retryGithubCall, the key is primarily for logging context, as we're not caching the paginated result directly here.
		// We can make a descriptive key.
		key := fmt.Sprintf("pr-list-page-%s-%s-page%d", org, project, currentOpts.Page)
		callDesc := fmt.Sprintf("PullRequests.List page %d for %s/%s (State: %s, Sort: %s, Direction: %s)", currentOpts.Page, org, project, listOpts.State, listOpts.Sort, listOpts.Direction)

		apiCall := func() (interface{}, *github.Response, error) {
			pagePRs, ghResp, err := ghClient.PullRequests.List(ctx, org, project, &currentOpts)
			return pagePRs, ghResp, err
		}

		rawData, ghResp, err := RetryGithubCall(ctx, key, callDesc, apiCall)
		if err != nil {
			return nil, err // Error is already formatted by retryGithubCall
		}

		pagePRs, ok := rawData.([]*github.PullRequest)
		if !ok {
			return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected []*github.PullRequest)", callDesc, rawData)
		}

		allPRs = append(allPRs, pagePRs...)

		if ghResp.NextPage == 0 {
			break
		}
		currentOpts.Page = ghResp.NextPage
	}
	return allPRs, nil
}
