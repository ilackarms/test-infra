/*
Copyright 2019 The Knative Authors

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

package main

import (
	"fmt"
	"log"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/github"

	"github.com/knative/test-infra/shared/ghutil"
	"github.com/knative/test-infra/shared/junit"
)

const (
	// flakyLabel is the Github issue label used for querying all flaky issues auto-generated.
	flakyLabel          = "auto:flaky"
	testIdentifierToken = "DONT_MODIFY_TEST_IDENTIFIER"
	latestStatusToken   = "Latest result for this test: "
	beforeHistoryToken  = "<!------Latest History of Up To 10 runs------>"
	afterHistoryToken   = "<!------End of History------>"
	jobLogsURL          = "https://prow.knative.dev/view/gcs/knative-prow/logs/"
	daysConsiderOld     = 30 // arbitrary number of days for an issue to be considered old
	maxHistoryEntries   = 10 // max count of history runs to show in unicode graph

	passedUnicode  = "&#10004;" // Checkmark unicode
	failedUnicode  = "&#10006;" // Cross unicode
	skippedUnicode = "&#9723;"  // Open square unicode

	// collapseTemplate is for creating collapsible format in comment, so that only latest result is expanded.
	// Github markdown supports collapse, strings between <summary></summary> is the title,
	// contents between <p></p> is collapsible body, body is hidden unless the title is clicked.
	collapseTemplate = `
<details>
	<summary>%s</summary><p>

%s
</p></details>`  // The blank line is crucial for collapsible format

	// issueBodyTemplate is a template for issue body
	issueBodyTemplate = `
### Auto-generated issue tracking flakiness of test
* **Test name**: %s
* **Repository name**: %s

<!-------------End of issue body, Please don't edit below this line------------->
<!--%s-->`
)

var (
	// testIdentifierPattern is used for formatting test identifier,
	// expect an argument of test identifier
	testIdentifierPattern = fmt.Sprintf("[%[1]s]%%s[%[1]s]", testIdentifierToken)
	// reTestIdentifier is regex matching pattern for capturing testname
	reTestIdentifier = fmt.Sprintf(`\[%[1]s\](.*?)\[%[1]s\]`, testIdentifierToken)

	// historyPattern is for creating history section in commment,
	// expect an argument of history Unicode from previous comment
	historyPattern = fmt.Sprintf("\n%s%%s%s\n%s Passed\t%s Failed\t%s Skipped\n",
		beforeHistoryToken, afterHistoryToken, passedUnicode, failedUnicode, skippedUnicode)
	// reHistory is for identifying history from comment
	reHistory = fmt.Sprintf("(?s)%s(.*?)%s", beforeHistoryToken, afterHistoryToken)

	// latestStatusPattern is for creating latest test result line in comment,
	// expect an argument of test status as defined in result.go
	latestStatusPattern = fmt.Sprintf(`%s%%s`, latestStatusToken)

	// reLastestStatus is for identifying latest test result from comment
	reLastestStatus = fmt.Sprintf(`%s([a-zA-Z]*)`, latestStatusToken)

	// Precompute timeConsiderOld so that the same standard used everywhere
	timeConsiderOld = time.Now().AddDate(0, 0, -daysConsiderOld)
)

// flakyIssue is a wrapper of github.Issue, used for storing pre-computed information
type flakyIssue struct {
	issue    *github.Issue
	identity *string              // identity discovered by matching reTestIdentifier
	comment  *github.IssueComment // The first auto comment, updated for every history
}

// getIdentityForTest creates a unique string for a test, which will be used for identifying Github issue
func getIdentityForTest(testFullName, repoName string) string {
	return fmt.Sprintf("'%s' in repo '%s'", testFullName, repoName)
}

// getBulkIssueIdentity creates a unique identity for a bulk issue when the flaky rate is above a threshold
func getBulkIssueIdentity(rd *RepoData, flakyRate float32) string {
	return fmt.Sprintf("%.2f%% tests failed in repo %s on %s",
		flakyRate*100, rd.Config.Repo, time.Unix(*rd.LastBuildStartTime, 0).String())
}

// GithubIssue handles methods for github issues
type GithubIssue struct {
	user   *github.User
	client ghutil.GithubOperations
}

// Setup creates the necessary setup to make calls to work with github issues
func Setup(githubToken string) (*GithubIssue, error) {
	ghc, err := ghutil.NewGithubClient(githubToken)
	if err != nil {
		return nil, fmt.Errorf("Cannot authenticate to github: %v", err)
	}

	ghUser, err := ghc.GetGithubUser()
	if nil != err {
		return nil, fmt.Errorf("Cannot get username: %v", err)
	}
	return &GithubIssue{user: ghUser, client: ghc}, nil
}

// The Repo field of an github Issue could be empty, use URL is more reliable
func getRepoFromIssue(issue *github.Issue) string {
	_, repo := path.Split(*(issue.RepositoryURL))
	return repo
}

// createCommentForTest summarizes latest status of current test case,
// and creates text to be added to issue comment
func (gi *GithubIssue) createCommentForTest(rd *RepoData, testFullName string) string {
	ts := rd.TestStats[testFullName]
	totalCount := len(ts.Passed) + len(ts.Skipped) + len(ts.Failed)
	lastBuildStartTimeStr := time.Unix(*rd.LastBuildStartTime, 0).String()
	content := fmt.Sprintf("%s\nLast build start time: %s\nFailed %d times out of %d runs.",
		fmt.Sprintf(latestStatusPattern, ts.getTestStatus()),
		lastBuildStartTimeStr, len(ts.Failed), totalCount)
	if len(ts.Failed) > 0 {
		content += " Failed runs: "
		var buildIDContents []string
		for _, buildID := range ts.Failed {
			buildIDContents = append(buildIDContents,
				fmt.Sprintf("[%d](%s%s/%d)", buildID, jobLogsURL, rd.Config.Name, buildID))
		}
		content += strings.Join(buildIDContents, ", ")
	}
	return content
}

func (gi *GithubIssue) createHistoryUnicode(rd *RepoData, comment, testFullName string) string {
	res := ""
	currentUnicode := fmt.Sprintf("%s: ", time.Unix(*rd.LastBuildStartTime, 0).String())
	resultSlice := rd.getResultSliceForTest(testFullName)
	for i, buildID := range rd.BuildIDs {
		url := fmt.Sprintf("%s%s/%d", jobLogsURL, rd.Config.Name, buildID)
		var statusUnicode string
		switch resultSlice[i] {
		case junit.Passed:
			statusUnicode = passedUnicode
		case junit.Failed:
			statusUnicode = failedUnicode
		default:
			statusUnicode = skippedUnicode
		}
		currentUnicode += fmt.Sprintf(" [%s](%s)", statusUnicode, url)
	}

	oldHistory := regexp.MustCompile(reHistory).FindStringSubmatch(comment)
	res = fmt.Sprintf("\n%s", currentUnicode)
	if len(oldHistory) >= 2 {
		oldHistoryEntries := strings.Split(oldHistory[1], "\n")
		if len(oldHistoryEntries) >= maxHistoryEntries {
			oldHistoryEntries = oldHistoryEntries[:(maxHistoryEntries - 1)]
		}
		res += strings.Join(oldHistoryEntries, "\n")
	} else {
		res += "\n"
	}

	return fmt.Sprintf(historyPattern, res)
}

// prependComment hides old comment into a collapsible, and prepend
// new commment on top
func (gi *GithubIssue) prependComment(oldComment, newComment string) string {
	if "" != oldComment {
		oldComment = fmt.Sprintf(collapseTemplate, "Click to see older results", oldComment)
	}
	return fmt.Sprintf("%s\n\n%s", newComment, oldComment)
}

// updateIssue adds comments to an existing issue, close an issue if test passed both in previous day and today,
// reopens the issue if test becomes flaky while issue is closed.
func (gi *GithubIssue) updateIssue(fi *flakyIssue, newComment string, ts *TestStat, dryrun bool) error {
	issue := fi.issue
	passedLastTime := false
	latestStatus := regexp.MustCompile(reLastestStatus).FindStringSubmatch(fi.comment.GetBody())
	if len(latestStatus) >= 2 {
		switch latestStatus[1] {
		case passedStatus:
			passedLastTime = true
		case flakyStatus, failedStatus, lackDataStatus:

		default:
			return fmt.Errorf("invalid test status code found from issue '%s'", *issue.URL)
		}
	}

	// Update comment unless test passed and issue closed
	if !ts.isPassed() || issue.GetState() == string(ghutil.IssueOpenState) {
		if err := run(
			"updating comment",
			func() error {
				return gi.client.EditComment(org, getRepoFromIssue(issue), *fi.comment.ID, gi.prependComment(*fi.comment.Body, newComment))
			},
			dryrun); nil != err {
			return fmt.Errorf("failed updating comments for issue '%s': '%v'", *issue.URL, err)
		}
	}

	if ts.isPassed() { // close open issue if the test passed twice consecutively
		if issue.GetState() == string(ghutil.IssueOpenState) {
			if passedLastTime {
				if err := run(
					"closing issue",
					func() error {
						closeErr := gi.client.CloseIssue(org, getRepoFromIssue(issue), *issue.Number)
						if nil == closeErr {
							closeComment := "Closing issue: this test has passed in latest 2 scans"
							_, closeErr = gi.client.CreateComment(org, getRepoFromIssue(issue), *issue.Number, closeComment)
						}
						return closeErr
					},
					dryrun); nil != err {
					return fmt.Errorf("failed closing issue '%s': '%v'", *issue.URL, err)
				}
			}
		}
	} else if ts.isFlaky() { // reopen closed issue if test found flaky
		if issue.GetState() == string(ghutil.IssueCloseState) {
			if err := run(
				"reopening issue",
				func() error {
					openErr := gi.client.ReopenIssue(org, getRepoFromIssue(issue), *issue.Number)
					if nil == openErr {
						openComment := "Reopening issue: this test is flaky"
						_, openErr = gi.client.CreateComment(org, getRepoFromIssue(issue), *issue.Number, openComment)
					}
					return openErr
				},
				dryrun); nil != err {
				return fmt.Errorf("failed reopen issue: '%s'", *issue.URL)
			}
		}
	}
	return nil
}

// createNewIssue creates an issue, adds flaky label and adds comment.
func (gi *GithubIssue) createNewIssue(org, repoForIssue, title, body string, comment string, dryrun bool) error {
	var newIssue *github.Issue
	if err := run(
		"creating issue",
		func() error {
			var err error
			newIssue, err = gi.client.CreateIssue(org, repoForIssue, title, body)
			return err
		},
		dryrun,
	); nil != err {
		return fmt.Errorf("failed creating issue '%s' in repo '%s'", title, repoForIssue)
	}
	var addIdentityErrs []error // clean up issue if any error occurred during adding identity, see below
	if err := run(
		"adding comment",
		func() error {
			_, err := gi.client.CreateComment(org, repoForIssue, *newIssue.Number, comment)
			return err
		},
		dryrun,
	); nil != err {
		addIdentityErrs = append(addIdentityErrs, fmt.Errorf("failed adding comment to issue '%s', '%v'", *newIssue.URL, err))
	}
	if nil == combineErrors(addIdentityErrs) {
		if err := run(
			"adding flaky label",
			func() error {
				return gi.client.AddLabelsToIssue(org, repoForIssue, *newIssue.Number, []string{flakyLabel})
			},
			dryrun,
		); nil != err {
			addIdentityErrs = append(addIdentityErrs, fmt.Errorf("failed adding '%s' label to issue '%s', '%v'", flakyLabel, *newIssue.URL, err))
		}
	}
	// This tool is designed to ensure a very small pool of issues related to flaky tests, by minimizing
	// chances of duplicate issues. If for any reason the created issue failed to be labeled with correct identities,
	// this issue will be invalid, and it's very likely that the same issue will be created the next time around.
	// So cleanup issue if failed adding identity, by removing flaky label and closing issue
	if nil != combineErrors(addIdentityErrs) {
		if err := run(
			"cleaning up invalid issue",
			func() error {
				var gErr []error
				if rlErr := gi.client.RemoveLabelForIssue(org, repoForIssue, *newIssue.Number, flakyLabel); nil != rlErr {
					gErr = append(gErr, rlErr)
				}
				if cErr := gi.client.CloseIssue(org, repoForIssue, *newIssue.Number); nil != cErr {
					gErr = append(gErr, cErr)
				}
				return combineErrors(gErr)
			},
			dryrun,
		); nil != err {
			addIdentityErrs = append(addIdentityErrs, err)
		}
	}
	return combineErrors(addIdentityErrs)
}

// findExistingComment identify existing comment by comment author and test identifier,
// if multiple comments were found return the earliest one.
func (gi *GithubIssue) findExistingComment(issue *github.Issue, issueIdentity string) (*github.IssueComment, error) {
	var targetComment *github.IssueComment
	comments, err := gi.client.ListComments(org, getRepoFromIssue(issue), *issue.Number)
	if nil != err {
		return nil, err
	}
	sort.Slice(comments, func(i, j int) bool {
		return nil != comments[i].CreatedAt && (nil == comments[j].CreatedAt || comments[i].CreatedAt.Before(*comments[j].CreatedAt))
	})

	for i, comment := range comments {
		if *comment.User.ID != *gi.user.ID {
			continue
		}
		if testNameFromComment := regexp.MustCompile(reTestIdentifier).FindStringSubmatch(*comment.Body); len(testNameFromComment) >= 2 && issueIdentity == testNameFromComment[1] {
			targetComment = comments[i]
			break
		}
	}
	if nil == targetComment {
		return nil, fmt.Errorf("no comment match")
	}
	return targetComment, nil
}

// getFlakyIssues filters all issues by flakyLabel, and return map {testName: slice of issues}
// Fail if find any issue with no discoverable identifier(testIdentifierPattern missing),
// also fail if auto comment not found.
// In most cases there is only 1 issue for each testName, if multiple issues found open for same test,
// most likely it's caused by old issues being reopened manually, in this case update both issues.
func (gi *GithubIssue) getFlakyIssues() (map[string][]*flakyIssue, error) {
	issuesMap := make(map[string][]*flakyIssue)
	reposForIssue, err := gi.client.ListRepos(org)
	if nil != err {
		return nil, err
	}
	for _, repoForIssue := range reposForIssue {
		issues, err := gi.client.ListIssuesByRepo(org, repoForIssue, []string{flakyLabel})
		if nil != err {
			return nil, err
		}
		for _, issue := range issues {
			if nil != issue.ClosedAt && issue.ClosedAt.Before(timeConsiderOld) {
				continue // Issue closed long time ago, it might fail with a different reason now.
			}
			issueIdentity := regexp.MustCompile(reTestIdentifier).FindStringSubmatch(issue.GetBody())
			if len(issueIdentity) < 2 { // Malformed issue, all auto flaky issues need to be identifiable.
				return nil, fmt.Errorf("cannot get test identifier from auto flaky issue '%v'", err)
			}
			autoComment, err := gi.findExistingComment(issue, issueIdentity[1])
			if nil != err {
				return nil, fmt.Errorf("cannot find auto comment for issue '%s': '%v'", *issue.URL, err)
			}
			issuesMap[issueIdentity[1]] = append(issuesMap[issueIdentity[1]], &flakyIssue{
				issue:    issue,
				identity: &issueIdentity[1],
				comment:  autoComment,
			})
		}
	}
	// Handle test with multiple issues associated
	// if all open: update all of them
	// if all closed: only keep latest one
	// otherwise(these may have been closed manually): remove closed ones from map
	for k, v := range issuesMap {
		hasOpen := false
		hasClosed := false
		for _, fi := range v {
			switch fi.issue.GetState() {
			case string(ghutil.IssueOpenState):
				hasOpen = true
			case string(ghutil.IssueCloseState):
				hasClosed = true
			}
		}
		if hasOpen && hasClosed {
			for i, fi := range v {
				if string(ghutil.IssueCloseState) == fi.issue.GetState() {
					issuesMap[k] = append(issuesMap[k][:i], issuesMap[k][i+1:]...)
				}
			}
		} else if !hasOpen {
			sort.Slice(issuesMap[k], func(i, j int) bool {
				return nil != issuesMap[k][i].issue.CreatedAt &&
					(nil == issuesMap[k][j].issue.CreatedAt || issuesMap[k][i].issue.CreatedAt.Before(*issuesMap[k][j].issue.CreatedAt))
			})
			issuesMap[k] = []*flakyIssue{issuesMap[k][0]}
		}
	}
	return issuesMap, err
}

// processGithubIssueForRepo reads RepoData and existing issues, and create/close/reopen/comment on issues.
// The function returns a slice of messages containing performed actions, and a slice of error messages,
// these can later on be printed as summary at the end of run
func (gi *GithubIssue) processGithubIssueForRepo(rd *RepoData, flakyIssuesMap map[string][]*flakyIssue, dryrun bool) ([]string, error) {
	var messages []string
	var errs []error

	repoForIssue := rd.Config.IssueRepo
	if len(repoForIssue) == 0 {
		messages = append(messages, "skip creating/updating issues, job is marked to not create GitHub issues\n")
		return messages, combineErrors(errs)
	}

	// If there are too many failures, create a single issue tracking it.
	if flakyRateAboveThreshold(rd) {
		flakyRate := getFlakyRate(rd)
		log.Printf("flaky rate above threshold, creating a single issue")
		identity := getBulkIssueIdentity(rd, flakyRate)
		if _, ok := flakyIssuesMap[identity]; ok {
			log.Printf("issue already exist, skip creating")
			return nil, nil
		}
		title := fmt.Sprintf("[flaky] %s", identity)
		comment := fmt.Sprintf("Bulk issue tracking: %s\n<!--%s-->", identity, fmt.Sprintf(testIdentifierPattern, identity))
		message := fmt.Sprintf("Creating issue '%s' in repo '%s'", title, repoForIssue)
		log.Println(message)
		return []string{message}, gi.createNewIssue(org, repoForIssue, title,
			fmt.Sprintf(issueBodyTemplate, identity, rd.Config.Repo, fmt.Sprintf(testIdentifierPattern, identity)),
			comment, dryrun)
	}

	// Update/Create issues for flaky/used-to-be-flaky tests
	for testFullName, ts := range rd.TestStats {
		if !ts.isFlaky() && !ts.isPassed() {
			continue
		}
		identity := getIdentityForTest(testFullName, rd.Config.Repo)
		comment := gi.createCommentForTest(rd, testFullName)
		if existIssues, ok := flakyIssuesMap[identity]; ok { // update issue with current result
			for _, existIssue := range existIssues {
				if strings.Contains(existIssue.comment.GetBody(), comment) {
					log.Printf("skip updating issue '%s', as it already contains data for run '%d'\n",
						*existIssue.issue.URL, *rd.LastBuildStartTime)
					continue
				}
				comment += gi.createHistoryUnicode(rd, existIssue.comment.GetBody(), testFullName)
				message := fmt.Sprintf("Updating issue '%s' for '%s'", *existIssue.issue.URL, *existIssue.identity)
				log.Println(message)
				messages = append(messages, message)
				if err := gi.updateIssue(existIssue, comment, ts, dryrun); nil != err {
					log.Println(err)
					errs = append(errs, err)
				}
			}
		} else if ts.isFlaky() {
			title := fmt.Sprintf("[flaky] %s", testFullName)
			comment = fmt.Sprintf("%s%s\n<!--%s-->", comment, gi.createHistoryUnicode(rd, "", testFullName),
				fmt.Sprintf(testIdentifierPattern, identity))
			message := fmt.Sprintf("Creating issue '%s' in repo '%s'", title, repoForIssue)
			log.Println(message)
			messages = append(messages, message)
			if err := gi.createNewIssue(org, repoForIssue, title,
				fmt.Sprintf(issueBodyTemplate, testFullName, rd.Config.Repo, fmt.Sprintf(testIdentifierPattern, identity)),
				comment, dryrun); nil != err {
				log.Println(err)
				errs = append(errs, err)
			}
		}
	}
	return messages, combineErrors(errs)
}

// analyze all results, figure out flaky tests and processing existing auto:flaky issues
func (gi *GithubIssue) processGithubIssues(repoDataAll []*RepoData, dryrun bool) error {
	// map repo to jobs, and jobs to messages
	messagesMap := make(map[string]map[string][]string)
	// map repo to jobs, and jobs to errors
	errMap := make(map[string]map[string][]error)

	// Collect all flaky test issues from all knative repos, in case issues are moved around
	// Fail this job if data collection failed
	flakyGHIssuesMap, err := gi.getFlakyIssues()
	if nil != err {
		log.Fatalf("%v", err)
	}

	for _, rd := range repoDataAll {
		messages, err := gi.processGithubIssueForRepo(rd, flakyGHIssuesMap, dryrun)
		if _, ok := messagesMap[rd.Config.Repo]; !ok {
			messagesMap[rd.Config.Repo] = make(map[string][]string)
		}
		messagesMap[rd.Config.Repo][rd.Config.Name] = messages
		if nil != err {
			if _, ok := errMap[rd.Config.Repo]; !ok {
				errMap[rd.Config.Repo] = make(map[string][]error)
			}
			errMap[rd.Config.Repo][rd.Config.Name] = append(errMap[rd.Config.Repo][rd.Config.Name], err)
		}
	}

	// Print summaries
	summary := "Summary:\n"
	for _, rd := range repoDataAll {
		if messages, ok := messagesMap[rd.Config.Repo][rd.Config.Name]; ok {
			summary += fmt.Sprintf("Summary of job '%s' in repo '%s':\n", rd.Config.Name, rd.Config.Repo)
			summary += strings.Join(messages, "\n")
		}
		if errs, ok := errMap[rd.Config.Repo][rd.Config.Name]; ok {
			summary += fmt.Sprintf("Errors in job '%s' in repo '%s':\n%v", rd.Config.Name, rd.Config.Repo, combineErrors(errs))
		}
	}

	log.Println(summary)
	return nil
}
