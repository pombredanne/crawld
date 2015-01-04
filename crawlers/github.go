// Copyright 2014-2015 The DevMine authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package crawlers

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"code.google.com/p/goauth2/oauth"
	"github.com/golang/glog"
	"github.com/google/go-github/github"

	"github.com/DevMine/crawld/config"
)

const tag = "[Github Crawler]"

// apiCallFunc is the default prototype a function that calls the GitHub API
// must have. This is necessary because API calls are wrapped into a function
// that checks if the API call rate limit is reached or not and waits before
// doing the call again if the limit is reached.
type apiCallFunc func(gc *GitHubCrawler, args ...interface{}) (interface{}, error)

var (
	errTooManyCall      = errors.New("API rate limit exceeded")
	errUnavailable      = errors.New("resource unavailable")
	errRuntime          = errors.New("runtime error")
	errInvalidArgs      = errors.New("invalid arguments")
	errNilArg           = errors.New("nil argument")
	errInvalidParamType = errors.New("invalid parameter type")
)

// GitHubCrawler implements the Crawler interface.
type GitHubCrawler struct {
	config.CrawlerConfig

	cloneDir string
	client   *github.Client
	db       *sql.DB
}

// ensure that GitHubCrawler implements the Crawler interface
var _ Crawler = (*GitHubCrawler)(nil)

func NewGitHubCrawler(cfg config.CrawlerConfig, cloneDir string, db *sql.DB) (*GitHubCrawler, error) {
	if db == nil {
		return nil, errors.New("database session cannot be nil")
	}

	var httpClient *http.Client
	if len(strings.Trim(cfg.OAuthAccessToken, " ")) != 0 {
		t := &oauth.Transport{
			Token: &oauth.Token{AccessToken: cfg.OAuthAccessToken},
		}
		httpClient = t.Client()
	}
	client := github.NewClient(httpClient)

	return &GitHubCrawler{cfg, cloneDir, client, db}, nil
}

func (g *GitHubCrawler) Crawl() {
	if g.UseSearchAPI {
		for _, lang := range g.Languages {
			_ = g.call(true, fetchTopRepositories, lang)
		}
	} else {
		_ = g.call(false, fetchRepositories)
	}
}

// call shall be used when doing a query on the GitHub API. If the query is
// refused, typically because the rate limit is reached, then this function
// waits for the appropriate time before retrying the query.
// isSearchRequest shall be used to indicate if apiCallFunc calls the search API
// (rate limit for the search API differ from the core API).
func (g *GitHubCrawler) call(isSearchRequest bool, fct apiCallFunc, args ...interface{}) interface{} {
	var ret interface{}
	var err error

	// gotta wait if rate limit is exceeded
	for {
		if ret, err = fct(g, args...); err != errTooManyCall {
			break
		}

		var reset int64
		limits, _, _ := g.client.RateLimits()
		if isSearchRequest {
			reset = limits.Search.Reset.Unix()
		} else {
			reset = limits.Core.Reset.Unix()
		}
		waitTime := reset - time.Now().Unix() + 1
		glog.Infof("%s call: not enough API calls left. Waiting for %d minutes and %d seconds",
			tag, waitTime/60, waitTime%60)
		time.Sleep(time.Duration(waitTime) * time.Second)
	}

	return ret
}

// fetchRepositories fetches N GitHub repositories in the given
// language (if provided).
//
// Warning: This method does not use the search API, thus, it uses a lot of API
// calls.
//
// args expects no argument.
//
// TODO add doc => the limit N is global to all languages
func fetchRepositories(gc *GitHubCrawler, args ...interface{}) (interface{}, error) {
	if len(args) != 0 {
		glog.Error(tag, "fetchRepositories: invalid number of arguments")
		return nil, errInvalidArgs
	}

	n := gc.Limit

	keepFork := gc.Fork
	hasLimit := n > 0

	// GitHub lists repositories 100 per page, regardless of the per_page option...
	opt := &github.RepositoryListAllOptions{}

	var sinceID int
ResultsLoop:
	for {
		opt.Since = sinceID
		repos, resp, err := gc.client.Repositories.ListAll(opt)
		if err != nil {
			glog.Error(tag, "fetchRepositories:", err)
			return nil, genApiCallFuncError(resp, err)
		}

		if len(repos) == 0 {
			break
		}

		for _, repo := range repos {
			sinceID = *repo.ID

			if n == 0 && hasLimit {
				break ResultsLoop
			}

			// skip? fork repos
			if *repo.Fork && !keepFork {
				continue
			}

			if ok, err := isLanguageWanted(gc.Languages, repo.Language); err != nil {
				glog.Error(err)
				continue
			} else if !ok {
				langs := gc.call(false, fetchRepositoryLanguages, *repo.Owner.Login, *repo.Name)

				if ok, err := isLanguageWanted(gc.Languages, langs); err != nil {
					glog.Error(err)
					continue
				} else if !ok {
					continue
				}
			}

			var fullRepo *github.Repository
			tmpRepo := gc.call(false, fetchRepository, *repo.Owner.Login, *repo.Name)
			switch tmpRepo.(type) {
			case *github.Repository:
				fullRepo = tmpRepo.(*github.Repository)
			default:
				glog.Error("fetchRepositories: invalid fetched repository")
				continue
			}

			// skip when an the method fail because the repository is not
			// saved into the DB
			if !insertOrUpdateRepo(gc, fullRepo) {
				continue
			}

			n--
		}

		if n <= 0 && hasLimit {
			break
		}
	}
	return nil, nil
}

// fetchTopRepositories fetches top N GitHub repositories in the given
// language (if provided).
//
// Warning: This method uses the search API, thus it cannot fetch more than
// 1000 results.
//
// args expects 1 values:
//   - language: string indicating the programming language to limit the fetch
// Be very careful if you do not specify a limit and/or a programming language.
//
// TODO add doc => the limit N is for language separately
func fetchTopRepositories(gc *GitHubCrawler, args ...interface{}) (interface{}, error) {
	if len(args) != 1 {
		glog.Error(tag, "fetchTopRepositories: invalid number of arguments")
		return nil, errInvalidArgs
	}

	n := gc.Limit

	var lang string
	switch args[0].(type) {
	case string:
		lang = args[0].(string)
	default:
		glog.Errorf("%s fetchTopRepositories: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[1]))
		return nil, errInvalidParamType
	}

	keepFork := gc.Fork
	hasLimit := n > 0

	opt := &github.SearchOptions{Sort: "stars", ListOptions: github.ListOptions{PerPage: 100}}

ResultsLoop:
	for {
		results, resp, err := gc.client.Search.Repositories(
			"language:"+lang, opt)
		if err != nil {
			glog.Error(tag, "fetchTopRepositories:", err)
			return nil, genApiCallFuncError(resp, err)
		}

		repos := results.Repositories

		for _, repo := range repos {
			if n == 0 && hasLimit {
				break ResultsLoop
			}

			// skip? fork repos
			if *repo.Fork && !keepFork {
				continue
			}

			// skip when an the method fail because the repository is not
			// saved into the DB
			if !insertOrUpdateRepo(gc, &repo) {
				continue
			}

			n--
		}

		if resp.NextPage == 0 || (n <= 0 && hasLimit) {
			break
		}

		opt.Page = resp.NextPage
	}
	return nil, nil
}

// fetchRepositoryLanguages fetches all languages related to a repository
// args expects 2 values:
// - owner: the repository owner
// - rpeo: the repository name
//
// It returns a map of languages (map[string]int, language => num bytes)
func fetchRepositoryLanguages(gc *GitHubCrawler, args ...interface{}) (interface{}, error) {
	if len(args) != 2 {
		glog.Error(tag, "fetchRepositoryLanguages: invalid number of arguments")
		return nil, errInvalidArgs
	}

	var owner string
	switch args[0].(type) {
	case string:
		owner = args[0].(string)
	default:
		glog.Errorf("%s fetchRepositoryLanguages: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[1]))
		return nil, errInvalidParamType
	}

	var repo string
	switch args[1].(type) {
	case string:
		repo = args[1].(string)
	default:
		glog.Errorf("%s fetchRepositoryLanguages: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[1]))
		return nil, errInvalidParamType
	}

	langs, resp, err := gc.client.Repositories.ListLanguages(owner, repo)
	if err != nil {
		glog.Error("fetchRepositoryLanguages: ", err)
		return nil, genApiCallFuncError(resp, err)
	}

	return langs, nil
}

// fetchRepository fetches the information about a specific repository.
//
// args expects 2 values:
// - owner: the repository owner
// - rpeo: the repository name
//
// It returns a github.Repository
func fetchRepository(gc *GitHubCrawler, args ...interface{}) (interface{}, error) {
	if len(args) != 2 {
		glog.Error(tag, "fetchRepository: invalid number of arguments")
		return nil, errInvalidArgs
	}

	var owner string
	switch args[0].(type) {
	case string:
		owner = args[0].(string)
	default:
		glog.Errorf("%s fetchRepository: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[1]))
		return nil, errInvalidParamType
	}

	var repo string
	switch args[1].(type) {
	case string:
		repo = args[1].(string)
	default:
		glog.Errorf("%s fetchRepository: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[1]))
		return nil, errInvalidParamType
	}

	ghRepo, resp, err := gc.client.Repositories.Get(owner, repo)
	if err != nil {
		glog.Error("fetchRepository: ", err)
		return nil, genApiCallFuncError(resp, err)
	}

	return ghRepo, nil
}

// getRepoID returns the repository id of repo in repositories table.
// If repo is not in the table, then 0 is returned. If an error occurs, -1 is returned.
func getRepoID(gc *GitHubCrawler, repo *github.Repository) int {
	if repo == nil {
		glog.Warning(tag, "getRepoID: 'repo' arg given is nil")
		return -1
	}

	var id int
	err := gc.db.QueryRow("SELECT repository_id FROM gh_repositories WHERE github_id=$1", repo.ID).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		return 0
	case err != nil:
		glog.Error(tag, "getRepoID:", err)
		return -1
	}
	return id
}

// getGhRepoID returns the github repository id of repo in repositories table.
// If repo is not in the table, then 0 is returned. If an error occurs, -1 is returned.
func getGhRepoID(gc *GitHubCrawler, repo *github.Repository) int {
	if repo == nil {
		glog.Warning(tag, "getGhRepoID: 'repo' arg given is nil")
		return -1
	}

	var id int
	err := gc.db.QueryRow("SELECT id FROM gh_repositories WHERE github_id=$1", repo.ID).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		return 0
	case err != nil:
		glog.Error(tag, "getGhRepoID:", err)
		return -1
	}
	return id
}

// getGhOrgID returns the github organization id of org in gh_organizations table.
// If org is not in the table, then 0 is returned. If an error occurs, -1 is returned.
func getGhOrgID(gc *GitHubCrawler, org *github.Organization) int {
	if org == nil {
		glog.Warning(tag, "getGhOrgID: 'org' arg given is nil")
		return -1
	}

	var id int
	err := gc.db.QueryRow("SELECT id FROM gh_organizations WHERE github_id=$1", org.ID).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		return 0
	case err != nil:
		glog.Error(tag, "getGhOrgID:", err)
		return -1
	}
	return id
}

// getGhUserID returns the github user id of user in gh_users table.
// If user not in the table, then 0 is returned. If an error occurs, -1 is returned.
func getGhUserID(gc *GitHubCrawler, user *github.User) int {
	if user == nil {
		glog.Warning(tag, "getGhUserID: 'user' arg given is nil")
		return -1
	}

	var id int
	err := gc.db.QueryRow("SELECT id FROM gh_users WHERE github_id=$1", user.ID).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		return 0
	case err != nil:
		glog.Error(tag, "getGhUserID:", err)
		return -1
	}
	return id
}

// getUserID returns the github user id of user in users table.
// If user not in the table, then 0 is returned. If an error occurs, -1 is returned.
func getUserID(gc *GitHubCrawler, user *github.User) int {
	if user == nil {
		glog.Warning(tag, "getUserID: 'user' arg given is nil")
		return -1
	}

	var id int
	err := gc.db.QueryRow("SELECT user_id FROM gh_users WHERE github_id=$1", user.ID).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		return 0
	case err != nil:
		glog.Error(tag, "getUserID:", err)
		return -1
	}
	return id
}

// insertOrUpdateRepo "does it all", ie: it searches for repositories, according
// to the information in the configuration file, queries them and the
// organization (including organization members) if the repo owner is an
// organization, or simply the repo owner if this is a user.
func insertOrUpdateRepo(gc *GitHubCrawler, repo *github.Repository) bool {
	if repo == nil {
		glog.Warning(tag, "insertOrUpdateRepo: 'repo' arg given is nil")
		return false
	}
	glog.Infof("%s insert or update repository: %s", tag, *repo.Name)

	clonePath := strings.ToLower(*repo.Language) + "/" +
		strings.ToLower(*repo.Owner.Login) + "/" + strings.ToLower(*repo.Name)
	repoFields := []string{"name", "primary_language", "clone_url", "clone_path", "vcs"}

	var query string
	if id := getRepoID(gc, repo); id > 0 {
		query = genUpdateQuery("repositories", id, repoFields...)
	} else if id == 0 {
		query = genInsQuery("repositories", repoFields...)
	} else {
		return false
	}

	var repoID int64
	err := gc.db.QueryRow(query+" RETURNING id", repo.Name, repo.Language, repo.CloneURL, clonePath, "git").Scan(&repoID)
	if err != nil {
		glog.Error(tag, "insertOrUpdateRepo:", err)
		return false
	}

	if *repo.Owner.Type != "Organization" {
		if !insertOrUpdateUser(gc, repo.Owner.Login, repoID, 0) {
			return false
		}
	} else {
		if !insertOrUpdateGhOrg(gc, repo.Owner.Login, repoID) {
			return false
		}
	}

	if !insertOrUpdateGhRepo(gc, repoID, repo) {
		return false
	}

	return true
}

// insertOrUpdateGhRepo inserts, or updates, a github repository in the
// database.
func insertOrUpdateGhRepo(gc *GitHubCrawler, repoID int64, repo *github.Repository) bool {
	if repo == nil {
		glog.Warning(tag, "insertOrUpdateGhRepo: 'repo' arg given is nil")
		return false
	}
	glog.Infof("%s insert or update github repository: %s", tag, *repo.Name)

	var ghOrganizationID *int
	if repo.Organization != nil {
		ghOrganizationID = repo.Organization.ID
	}

	ghRepoFields := []string{
		"repository_id",
		"full_name",
		"description",
		"homepage",
		"fork",
		"github_id",
		"default_branch",
		"master_branch",
		"html_url",
		"forks_count",
		"open_issues_count",
		"stargazers_count",
		"subscribers_count",
		"watchers_count",
		"size_in_kb",
		"created_at",
		"updated_at",
		"pushed_at",
	}

	var query string
	if id := getGhRepoID(gc, repo); id > 0 {
		query = genUpdateQuery("gh_repositories", id, ghRepoFields...)
	} else if id == 0 {
		query = genInsQuery("gh_repositories", ghRepoFields...)
	} else {
		return false
	}

	_, err := gc.db.Exec(query,
		repoID,
		repo.FullName,
		repo.Description,
		repo.Homepage,
		repo.Fork,
		repo.ID,
		repo.DefaultBranch,
		repo.MasterBranch,
		repo.HTMLURL,
		repo.ForksCount,
		repo.OpenIssuesCount,
		repo.StargazersCount,
		repo.SubscribersCount,
		repo.WatchersCount,
		repo.Size,
		formatTimestamp(*repo.CreatedAt),
		formatTimestamp(*repo.UpdatedAt),
		formatTimestamp(*repo.PushedAt))

	if err != nil {
		glog.Error(tag, "insertOrUpdateGhRepo:", err)
		return false
	}

	if ghOrganizationID != nil {
		if !insertOrUpdateGhOrg(gc, repo.Organization.Login, repoID) {
			return false
		}
	}

	return true
}

// insertOrUpdateGhOrg inserts, or updates, a github organization into
// the database.
func insertOrUpdateGhOrg(gc *GitHubCrawler, orgName *string, repoID int64) bool {
	if orgName == nil {
		glog.Warning(tag, "insertOrUpdateGhOrg: 'orgName' arg given is nil")
		return false
	}
	glog.Infof("%s insert or update github organization: %s", tag, *orgName)

	tmp := gc.call(false, fetchOrganization, *orgName)
	var org *github.Organization
	switch tmp.(type) {
	case *github.Organization:
		org = tmp.(*github.Organization)
	default:
		glog.Error(tag, "insertOrUpdateGhOrg: invalid function return type")
		return false
	}

	ghOrgFields := []string{
		"login",
		"github_id",
		"avatar_url",
		"html_url",
		"name",
		"company",
		"blog",
		"location",
		"email",
		"collaborators_count",
		"created_at",
		"updated_at",
	}

	var query string
	if id := getGhOrgID(gc, org); id > 0 {
		query = genUpdateQuery("gh_organizations", id, ghOrgFields...)
	} else if id == 0 {
		query = genInsQuery("gh_organizations", ghOrgFields...)
	} else {
		return false
	}

	var orgID int64
	err := gc.db.QueryRow(query+" RETURNING id",
		org.Login,
		org.ID,
		org.AvatarURL,
		org.HTMLURL,
		org.Name,
		org.Company,
		org.Blog,
		org.Location,
		org.Email,
		org.Collaborators,
		formatTimestamp(github.Timestamp{Time: *org.CreatedAt}),
		formatTimestamp(github.Timestamp{Time: *org.UpdatedAt})).Scan(&orgID)

	if err != nil {
		glog.Error(tag, "insertOrUpdateGhOrg:", err)
		return false
	}

	tmp = gc.call(false, fetchOrganizationMembers, *org.Login)
	var users []github.User
	switch tmp.(type) {
	case []github.User:
		users = tmp.([]github.User)
	default:
		glog.Error(tag, "insertOrUpdateGhOrg: invalid function return type")
	}

	for _, user := range users {
		if !insertOrUpdateUser(gc, user.Login, repoID, orgID) {
			return false
		}
	}

	return true
}

// insertOrUpdateUser inserts, or updates, a github user into the database.
func insertOrUpdateUser(gc *GitHubCrawler, username *string, repoID int64, orgID int64) bool {
	if username == nil {
		glog.Warning(tag, "insertOrUpdateGhOrg: 'username' arg given is nil")
		return false
	}
	glog.Infof("%s insert or update user: %s", tag, *username)

	if repoID <= 0 {
		glog.Warning(tag, "insertOrUpdateUser:",
			"trying to insert a user without linked GitHub repository")
		return false
	}

	tmp := gc.call(false, fetchUser, *username)
	var user *github.User
	switch tmp.(type) {
	case *github.User:
		user = tmp.(*github.User)
	default:
		glog.Error(tag, "insertOrUpdateUser: invalid function return type")
		return false
	}

	userFields := []string{"username", "name", "email"}

	var query string
	if id := getUserID(gc, user); id > 0 {
		query = genUpdateQuery("users", id, userFields...)
	} else if id == 0 {
		query = genInsQuery("users", userFields...)
	} else {
		return false
	}

	var userID int64
	err := gc.db.QueryRow(query+" RETURNING id", user.Login, user.Name, user.Email).Scan(&userID)
	if err != nil {
		glog.Error(tag, "insertOrUpdateUser:", err)
		return false
	}

	if !linkUserToRepo(gc.db, userID, repoID) {
		return false
	}

	if !insertOrUpdateGhUser(gc, userID, user, orgID) {
		return false
	}

	return true
}

// insertOrUpdateGhUser inserts, or updates, a github user into the database.
func insertOrUpdateGhUser(gc *GitHubCrawler, userID int64, user *github.User, orgID int64) bool {
	if user == nil {
		glog.Warning(tag, "insertOrUpdateGhOrg: 'user' arg given is nil")
		return false
	}
	glog.Infof("%s insert or update github user: %s", tag, *user.Login)

	if userID <= 0 {
		glog.Warning(tag, "insertOrUpdateGhUser:",
			"trying to insert a github user but no user ID given")
		return false
	}

	ghUserFields := []string{
		"user_id",
		"github_id",
		"login",
		"bio",
		"blog",
		"company",
		"email",
		"hireable",
		"location",
		"avatar_url",
		"html_url",
		"followers_count",
		"following_count",
		"collaborators_count",
		"created_at",
		"updated_at",
	}

	var query string
	if id := getGhUserID(gc, user); id > 0 {
		query = genUpdateQuery("gh_users", id, ghUserFields...)
	} else if id == 0 {
		query = genInsQuery("gh_users", ghUserFields...)
	} else {
		return false
	}

	var ghUserID int64
	err := gc.db.QueryRow(query+" RETURNING id",
		userID,
		user.ID,
		user.Login,
		user.Bio,
		user.Blog,
		user.Company,
		user.Email,
		user.Hireable,
		user.Location,
		user.AvatarURL,
		user.HTMLURL,
		user.Followers,
		user.Following,
		user.Collaborators,
		formatTimestamp(*user.CreatedAt),
		formatTimestamp(*user.UpdatedAt)).Scan(&ghUserID)

	if err != nil {
		glog.Error(tag, "insertOrUpdateGhUser:", err)
		return false
	}

	if orgID != 0 {
		if !linkGhUserToGhOrg(gc.db, ghUserID, orgID) {
			return false
		}
	}

	return true
}

// isUserLinkedToRepo checks whether a user is already linked to the given
// repository.
func isUserLinkedToRepo(db *sql.DB, userID, repoID int64) bool {
	row := db.QueryRow(
		`SELECT COUNT(*) AS total
		 FROM users_repositories
		 WHERE user_id = $1 AND repository_id = $2`, userID, repoID)

	var total int64
	if err := row.Scan(&total); err != nil {
		glog.Error(tag, "isUserLinkedToRepo:", err)
		return false
	}

	return total > 0
}

// linkUserToRepo creates a many to many relationship between a user and a
// repository.
func linkUserToRepo(db *sql.DB, userID, repoID int64) bool {
	if isUserLinkedToRepo(db, userID, repoID) {
		return true
	}

	fields := []string{"user_id", "repository_id"}

	query := genInsQuery("users_repositories", fields...)

	_, err := db.Exec(query, userID, repoID)
	if err != nil {
		glog.Error(tag, "linkUserToRepo:", err)
		return false
	}

	return true
}

// isGhUserLinkedToGhOrg checks whether a github user is linked to the given
// github organization or not.
func isGhUserLinkedToGhOrg(db *sql.DB, ghUserID, orgID int64) bool {
	row := db.QueryRow(
		`SELECT COUNT(*) AS total
		 FROM gh_users_organizations
		 WHERE gh_user_id = $1 AND gh_organization_id = $2`, ghUserID, orgID)

	var total int64
	if err := row.Scan(&total); err != nil {
		glog.Error(tag, "isGhUserLinkedToGhOrg:", err)
		return false
	}

	return total > 0
}

// linkGhUserToGhOrg links a github user to the given github organization.
func linkGhUserToGhOrg(db *sql.DB, ghUserID, orgID int64) bool {
	if isGhUserLinkedToGhOrg(db, ghUserID, orgID) {
		return true
	}

	fields := []string{"gh_user_id", "gh_organization_id"}

	query := genInsQuery("gh_users_organizations", fields...)

	_, err := db.Exec(query, ghUserID, orgID)
	if err != nil {
		glog.Error(tag, "linkGhUserToGhOrg:", err)
		return false
	}

	return true
}

// fetchOrganization fetches information about a github organization.
// args expects 1 value:
// - orgName: the organization name
func fetchOrganization(gc *GitHubCrawler, args ...interface{}) (interface{}, error) {
	if len(args) != 1 {
		glog.Error(tag, "fetchOrganization: invalid number of arguments")
		return nil, errInvalidArgs
	}

	var orgName string
	switch args[0].(type) {
	case string:
		orgName = args[0].(string)
	default:
		glog.Errorf("%s fetchOrganization: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[0]))
		return nil, errInvalidParamType
	}

	org, resp, err := gc.client.Organizations.Get(orgName)
	if err != nil {
		glog.Error(tag, "fetchOrganization:", err)
		return nil, genApiCallFuncError(resp, err)
	}

	return org, nil
}

// fetchUser fetches information about a user.
// args expects 1 value:
// - username: the user login name
func fetchUser(gc *GitHubCrawler, args ...interface{}) (interface{}, error) {
	if len(args) != 1 {
		glog.Error(tag, "fetchUser: invalid number of arguments")
		return nil, errInvalidArgs
	}

	var username string
	switch args[0].(type) {
	case string:
		username = args[0].(string)
	default:
		glog.Errorf("%s fetchUser: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[0]))
		return nil, errInvalidParamType
	}

	user, resp, err := gc.client.Users.Get(username)
	if err != nil {
		glog.Error(tag, "fetchUser:", err)
		return nil, genApiCallFuncError(resp, err)
	}

	return user, nil
}

// fetchContributors fetches all the contributors of a GitHub repository.
//
// args expects 2 values:
// - owner: the repository owner
// - repoName:  the repository name
//
// It returns a list of users.
func fetchContributors(gc *GitHubCrawler, args ...interface{}) (interface{}, error) {
	if len(args) != 2 {
		glog.Error(tag, "fetchContributors: invalid number of arguments")
		return nil, errInvalidArgs
	}

	var owner string
	switch args[0].(type) {
	case string:
		owner = args[0].(string)
	default:
		glog.Errorf("%s fetchConstributors: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[0]))
		return nil, errInvalidParamType
	}

	var repoName string
	switch args[1].(type) {
	case string:
		repoName = args[1].(string)
	default:
		glog.Errorf("%s fetchConstributors: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[1]))
		return nil, errInvalidParamType
	}

	users, resp, err := gc.client.Repositories.ListContributors(owner, repoName, nil)
	if err != nil {
		glog.Error(tag, "fetchContributors:", err)
		return nil, genApiCallFuncError(resp, err)
	}

	return users, nil
}

// fetchOrganizationMembers fetches all the members of a GitHub organization.
//
// args expects 1 values:
// - orgName: the organization name
//
// It returns a list of users.
func fetchOrganizationMembers(gc *GitHubCrawler, args ...interface{}) (interface{}, error) {
	if len(args) != 1 {
		glog.Error(tag, "fetchContributors: invalid number of arguments")
		return nil, errInvalidArgs
	}

	var orgName string
	switch args[0].(type) {
	case string:
		orgName = args[0].(string)
	default:
		glog.Errorf("%s fetchOrganizationMembers: invalid parameter type"+
			"(given %v, expected string)", tag, reflect.TypeOf(args[0]))
		return nil, errInvalidParamType
	}

	users, resp, err := gc.client.Organizations.ListMembers(orgName, nil)
	if err != nil {
		glog.Error(tag, "fetchOrganizationMembers:", err)
		return nil, genApiCallFuncError(resp, err)
	}

	return users, nil
}

// genInsQuery generates a query string for an insertion in the database.
func genInsQuery(tableName string, fields ...string) string {
	var buf bytes.Buffer

	buf.WriteString(fmt.Sprintf("INSERT INTO %s(%s)\n",
		tableName, strings.Join(fields, ",")))
	buf.WriteString("VALUES(")

	for ind, _ := range fields {
		if ind > 0 {
			buf.WriteString(",")
		}

		buf.WriteString(fmt.Sprintf("$%d", ind+1))
	}

	buf.WriteString(")\n")

	return buf.String()
}

// genUpdateQuery generates a query string for an update of fields in the
// database.
func genUpdateQuery(tableName string, id int, fields ...string) string {
	var buf bytes.Buffer

	buf.WriteString(fmt.Sprintf("UPDATE %s\n", tableName))
	buf.WriteString("SET ")

	for ind, field := range fields {
		if ind > 0 {
			buf.WriteString(",")
		}

		buf.WriteString(fmt.Sprintf("%s=$%d", field, ind+1))
	}

	buf.WriteString(fmt.Sprintf("WHERE id=%d\n", id))

	return buf.String()

}

// formatTimestamp formats a github.Timestamp to a string suitable to use
// as a timestamp with timezone PostgreSQL data type
func formatTimestamp(timeStamp github.Timestamp) string {
	return timeStamp.Format(time.RFC3339)
}

// isLanguageWanted checks if language(s) is in the list of wanted
// languages.
func isLanguageWanted(suppLangs []string, prjLangs interface{}) (bool, error) {
	if prjLangs == nil {
		return false, nil
	}

	switch prjLangs.(type) {
	case map[string]int:
		langs := prjLangs.(map[string]int)
		for k, _ := range langs {
			for _, v := range suppLangs {
				if strings.EqualFold(k, v) {
					return true, nil
				}
			}
		}
	case *string:
		lang := prjLangs.(*string)
		if lang == nil {
			return false, nil
		}

		for _, sl := range suppLangs {
			if sl == *lang {
				return true, nil
			}
		}
	default:
		return false, errors.New("isLanguageSupported: invalid prjLangs type")
	}

	return false, nil
}

// genApiCallFuncError creates an error base on the http response.
func genApiCallFuncError(resp *github.Response, err error) error {
	if resp == nil {
		glog.Warning(tag, "genApiCallFuncError: 'resp' arg given is nil")
		if err != nil {
			return err
		} else {
			return errNilArg
		}
	}

	if err == nil || resp.StatusCode != 403 {
		return err
	}

	switch {
	case strings.Contains(err.Error(), "API rate limit exceeded"):
		return errTooManyCall
	case strings.Contains(err.Error(), "access blocked"):
		return errUnavailable
	}

	return err
}
