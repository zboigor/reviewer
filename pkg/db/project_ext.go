package db

import "context"

// ProjectByGithubRepo returns an enabled Project matching the given GitHub owner and repo.
func (pr ProjectRepo) ProjectByGithubRepo(ctx context.Context, owner, repo string) (*Project, error) {
	statusID := StatusEnabled
	return pr.OneProject(ctx,
		&ProjectSearch{
			GithubOwner: &owner,
			GithubRepo:  &repo,
			StatusID:    &statusID,
		},
		pr.FullProject(),
	)
}
