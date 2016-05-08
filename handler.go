package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	maxWaitTime = 30 * time.Minute
	maxPollTime = 64 * time.Second
)

// The handler receives commands from the webhook
type handler struct {
	token       string
	username    string
	allowed     []string
	teamAllowed []string
	stop        chan struct{}
	pending     map[int]struct{}
	mut         sync.Mutex
	permissions
}

func newHandler(allowed []string, username, token string) *handler {
	return &handler{
		username: username,
		token:    token,
		allowed:  allowed,
		stop:     make(chan struct{}),
		pending:  make(map[int]struct{}),
		permissions: permissions{
			username:      username,
			token:         token,
			alwaysAllowed: allowed,
			teamMembers:   make(map[string][]string),
		},
	}
}

func (h *handler) handlePullReq(p pr) {
	h.mut.Lock()
	defer h.mut.Unlock()

	if _, err := os.Stat(filepath.Join(p.Repository.FullName, ".git")); err != nil {
		if err := clone(p.Repository.FullName); err != nil {
			log.Println(err)
			return
		}
	}

	cur, err := os.Getwd()
	if err != nil {
		log.Println("No working dir?")
		return
	}

	os.Chdir(p.Repository.FullName)

	switch p.Action {
	case "synchronize", "opened", "reopened":
		updatePR(p.Number)
		p.setStatus(stateSuccess, "st-review", "At your service.", h.username, h.token)
	case "closed":
		closePR(p.Number)
		p.setStatus(stateSuccess, "st-review", "Closed.", h.username, h.token)
	}

	os.Chdir(cur)
}

func (h *handler) handleStop(c comment) {
	h.mut.Lock()
	defer h.mut.Unlock()

	if !h.isAllowed(c.Repository.FullName, c.Sender.Login) {
		c.post(noAccessResponse(c), h.username, h.token)
		log.Println("Rejecting request by unknown user", c.Sender.Login)
		return
	}

	pr, err := c.getPR()
	if err != nil {
		log.Println("No pull request:", err)
		return
	}

	pr.setStatus(stateFailure, "st-review", "Not to be merged as is.", h.username, h.token)
	c.post(notMergingResponse(c), h.username, h.token)
}

func (h *handler) handleMerge(c comment) {
	h.mut.Lock()
	defer h.mut.Unlock()

	if !h.isAllowed(c.Repository.FullName, c.Sender.Login) {
		c.post(noAccessResponse(c), h.username, h.token)
		log.Println("Rejecting request by unknown user", c.Sender.Login)
		return
	}

	if _, ok := h.pending[c.Issue.Number]; ok {
		c.post(alreadyPendingResponse(c), h.username, h.token)
		log.Println("Rejecting request for already pending PR")
		return
	}

	pr, err := c.getPR()
	if err != nil {
		log.Println("No pull request:", err)
		return
	}

	status := overallStatus(pr.getStatuses(h.username, h.token))
	switch status {
	case stateSuccess:
		h.performMerge(c, pr)

	case statePending:
		c.post(waitingResponse(c), h.username, h.token)
		h.pending[c.Issue.Number] = struct{}{}
		go h.delayedMerge(c, pr)

	default:
		c.post(badBuildResponse(c, status), h.username, h.token)
	}
}

func (h *handler) delayedMerge(c comment, pr pr) {
	defer func() {
		h.mut.Lock()
		delete(h.pending, c.Issue.Number)
		h.mut.Unlock()
	}()

	t0 := time.Now()
	wait := time.Second

	for time.Since(t0) < maxWaitTime {
		status := overallStatus(pr.getStatuses(h.username, h.token))

		switch status {
		case stateSuccess:
			h.performMerge(c, pr)
			return
		case stateError, stateFailure:
			c.post(badBuildResponse(c, status), h.username, h.token)
			return
		}

		time.Sleep(wait)
		if wait < maxPollTime {
			wait *= 2
		}
	}

	c.post(timeoutResponse(c, maxWaitTime), h.username, h.token)
}

func (h *handler) performMerge(c comment, pr pr) {
	log.Printf("Attemping merge of PR %d on %s for %s", c.Issue.Number, c.Repository.FullName, c.Sender.Login)

	if _, err := os.Stat(filepath.Join(c.Repository.FullName, ".git")); err != nil {
		if err := clone(c.Repository.FullName); err != nil {
			log.Println(err)
			c.post(cloneFailedResponse(c, err.Error()), h.username, h.token)
			return
		}
	}

	cur, err := os.Getwd()
	if err != nil {
		log.Println("No working dir?")
		return
	}

	overrideDescr := ""
	body := c.parseBody()

	if body.subject != "" {
		overrideDescr = strings.TrimSpace(body.subject + "\n\n" + reflow(body.description, 76))
	}

	user, err := c.user(h.username, h.token)
	if err != nil || user.Email == "" {
		c.post(noUserResponse(c), h.username, h.token)
		log.Printf("Failed merge of PR %d on %s for %s: no user info (%v)", c.Issue.Number, c.Repository.FullName, c.Sender.Login, err)
		return
	}

	os.Chdir(c.Repository.FullName)
	sha1, err := squash(pr, user, overrideDescr)
	os.Chdir(cur)

	if err != nil {
		c.post(errorResponse(c, err.Error()), h.username, h.token)
		log.Printf("Failed merge of PR %d on %s for %s:\n%s", c.Issue.Number, c.Repository.FullName, c.Sender.Login, err.Error())

		return
	}

	c.post(thanksResponse(c, sha1), h.username, h.token)
	c.close(h.username, h.token)
	log.Printf("Completed merge of PR %d on %s for %s", c.Issue.Number, c.Repository.FullName, c.Sender.Login)
}

var allowedCommitSubjectRe = regexp.MustCompile(`^[a-zA-Z0-9_./-]+:\s`)

func squash(pr pr, user user, msg string) (string, error) {
	sourceBranch := fmt.Sprintf("pr-%d", pr.Number)
	s := newScript()
	s.run("git", "fetch", "-f", "origin", fmt.Sprintf("refs/pull/%d/head:pr-%d", pr.Number, pr.Number))
	s.run("git", "fetch", "-f", "origin", "master:orig/master")

	s.run("git", "reset", "--hard")
	s.run("git", "checkout", "master")
	s.run("git", "reset", "--hard", "orig/master")
	s.run("git", "clean", "-fxd")

	// Find first commit and extract info from it
	t := newScript()
	mergeBase := t.run("git", "merge-base", sourceBranch, "master")
	revs := strings.Fields(t.run("git", "rev-list", mergeBase+".."+sourceBranch))
	if len(revs) == 0 {
		return "", fmt.Errorf("Nothing to merge, as far as I can tell.")
	}
	firstCommit := revs[len(revs)-1]
	authorName := t.run("git", "log", "-n1", "--pretty=format:%an", firstCommit)
	authorEmail := t.run("git", "log", "-n1", "--pretty=format:%ae", firstCommit)
	os.Setenv("GIT_COMMITTER_NAME", user.Name)
	os.Setenv("GIT_COMMITTER_EMAIL", user.Email)
	os.Setenv("GIT_AUTHOR_NAME", authorName)
	os.Setenv("GIT_AUTHOR_EMAIL", authorEmail)

	var body string
	if msg != "" {
		// Overridden commit message from parameters
		body = msg
	} else {
		// Commit message from first commit
		body = t.run("git", "log", "-n1", "--pretty=format:%B", firstCommit)
	}

	body = fmt.Sprintf("%s\n\nGitHub-Pull-Request: %s\n", strings.TrimSpace(body), pr.HTMLURL)

	s.run("git", "merge", "--squash", "--no-commit", sourceBranch)
	s.runPipe(bytes.NewBufferString(body), "git", "commit", "-F", "-")
	sha1 := s.run("git", "rev-parse", "HEAD")
	s.run("git", "push", "origin", "master")

	if s.Error() != nil {
		// Overwrite the error with whatever actual output we had, as a markdown verbatim.
		return "", fmt.Errorf("%s", s.output.String())
	}
	return sha1, nil
}

func updatePR(pr int) {
	s := newScript()
	s.run("git", "fetch", "-f", "origin", fmt.Sprintf("refs/pull/%d/head:pr-%d", pr, pr))
	s.run("git", "push", "-f", "origin", fmt.Sprintf("pr-%d", pr))
}

func closePR(pr int) {
	s := newScript()
	s.run("git", "push", "origin", fmt.Sprintf(":pr-%d", pr))
}

func clone(repo string) error {
	s := newScript()
	s.run("git", "clone", fmt.Sprintf("git@github.com:%s.git", repo), repo)
	if s.Error() != nil {
		return fmt.Errorf("%s", s.output.String())
	}
	return nil
}
