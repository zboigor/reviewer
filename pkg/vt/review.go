package vt

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"reviewsrv/pkg/db"
	"reviewsrv/pkg/worker"

	"github.com/vmkteam/embedlog"
	"github.com/vmkteam/zenrpc/v2"
)

var prURLRe = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// parsePRURL extracts owner, repo and PR number from a GitHub PR URL.
func parsePRURL(s string) (owner, repo string, pr int, err error) {
	m := prURLRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", 0, fmt.Errorf("not a github PR URL: %q", s)
	}
	pr, _ = strconv.Atoi(m[3])
	return m[1], m[2], pr, nil
}

// ReviewService provides RPC methods for triggering code reviews.
type ReviewService struct {
	zenrpc.Service
	embedlog.Logger

	repo  db.ProjectRepo
	revs  db.ReviewRepo
	queue *worker.Queue
}

// NewReviewService returns a new ReviewService.
func NewReviewService(dbo db.DB, queue *worker.Queue, logger embedlog.Logger) *ReviewService {
	return &ReviewService{
		Logger: logger,
		repo:   db.NewProjectRepo(dbo),
		revs:   db.NewReviewRepo(dbo),
		queue:  queue,
	}
}

// Trigger creates a review for the given PR URL and enqueues a worker job.
// Integration coverage of the full path is deferred to the Task 15 smoke test.
//
//zenrpc:prUrl GitHub PR URL like https://github.com/owner/repo/pull/123
//zenrpc:return Created review ID
//zenrpc:400 Invalid URL or no project for repo
//zenrpc:401 Unauthenticated
func (s ReviewService) Trigger(ctx context.Context, prUrl string) (int, error) {
	user := UserFromContext(ctx)
	if user == nil {
		return 0, ErrUnauthorized
	}

	owner, repo, prNum, err := parsePRURL(prUrl)
	if err != nil {
		return 0, zenrpc.NewStringError(http.StatusBadRequest, err.Error())
	}

	project, err := s.repo.ProjectByGithubRepo(ctx, owner, repo)
	if err != nil {
		return 0, InternalError(err)
	}
	if project == nil {
		return 0, zenrpc.NewStringError(http.StatusBadRequest, "no project configured for "+owner+"/"+repo)
	}

	review := &db.Review{
		ProjectID:         project.ID,
		TriggeredByUserID: &user.ID,
		PRNumber:          &prNum,
		StatusID:          db.StatusEnabled,
	}
	if _, err := s.revs.AddReview(ctx, review); err != nil {
		return 0, InternalError(err)
	}

	if _, err := s.queue.Enqueue(ctx, review.ID); err != nil {
		return 0, InternalError(err)
	}

	return review.ID, nil
}
