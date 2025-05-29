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

	"github.com/google/go-github/v72/github"
	"k8s.io/klog/v2"

	"github.com/google/pullsheet/pkg/client"
	"github.com/google/pullsheet/pkg/ghcache"
)

// fetchAllRepoPages fetches all pages of repositories for an organization.
func fetchAllRepoPages(
	ctx context.Context,
	ghClient *github.Client,
	org string,
	listOpts *github.RepositoryListByOrgOptions,
) ([]*github.Repository, error) {
	var allGithubRepos []*github.Repository
	currentOpts := *listOpts // Make a copy to modify Page

	if currentOpts.Page == 0 {
		currentOpts.Page = 1
	}

	for {
		key := fmt.Sprintf("list-repo-pages-%s-page%d", org, currentOpts.Page)
		callDesc := fmt.Sprintf("Repositories.ListByOrg page %d for %s (Type: %s, Sort: %s, Direction: %s)", currentOpts.Page, org, listOpts.Type, listOpts.Sort, listOpts.Direction)

		apiCall := func() (interface{}, *github.Response, error) {
			pageReposData, ghRespData, errData := ghClient.Repositories.ListByOrg(ctx, org, &currentOpts)
			return pageReposData, ghRespData, errData
		}

		rawData, ghResp, err := ghcache.RetryGithubCall(ctx, key, callDesc, apiCall)
		if err != nil {
			return nil, err // Error is already formatted by RetryGithubCall
		}

		pageRepos, ok := rawData.([]*github.Repository)
		if !ok {
			return nil, fmt.Errorf("unexpected type from GitHub API for %s: %T (expected []*github.Repository)", callDesc, rawData)
		}

		if len(pageRepos) == 0 {
			klog.V(1).Infof("No repositories found on page %d for %s with specified criteria. Ending pagination.", currentOpts.Page, org)
			break
		}

		allGithubRepos = append(allGithubRepos, pageRepos...)

		if ghResp.NextPage == 0 {
			break
		}
		currentOpts.Page = ghResp.NextPage
	}
	klog.V(1).Infof("Fetched %d repositories via paginated API calls for org %s before any further processing.", len(allGithubRepos), org)
	return allGithubRepos, nil
}

// ListRepoNames returns the names of all the repositories of the specified Github organization.
func ListRepoNames(ctx context.Context, c *client.Client, org string) ([]string, error) {
	klog.Infof("Listing repositories for organization %s...", org)
	// Retrieve all the repositories of the specified Github organization
	opts := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: 100}, // Increased PerPage for efficiency
	}

	githubRepos, err := fetchAllRepoPages(ctx, c.GitHubClient, org, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch repository list for organization %s: %w", org, err)
	}

	if len(githubRepos) == 0 {
		klog.Infof("No repositories found for organization %s.", org)
		return []string{}, nil
	}

	var repoNames []string
	for _, repo := range githubRepos {
		repoNames = append(repoNames, org+"/"+repo.GetName())
	}

	klog.Infof("Returning %d repository names for organization %s.", len(repoNames), org)
	return repoNames, nil
}
