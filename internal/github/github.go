package github

type GithubPullRequestAction string

const (
	ActionOpened      GithubPullRequestAction = "opened"
	ActionReopened    GithubPullRequestAction = "reopened"
	ActionClosed      GithubPullRequestAction = "closed"
	ActionSynchronize GithubPullRequestAction = "synchronize"
)

type GithubPullRequestEvent struct {
	Action GithubPullRequestAction `json:"action"`
}
