package services

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/google/go-github/github"
	"github.com/matrix-org/go-neb/database"
	"github.com/matrix-org/go-neb/matrix"
	"github.com/matrix-org/go-neb/plugin"
	"github.com/matrix-org/go-neb/services/github/client"
	"github.com/matrix-org/go-neb/services/github/webhook"
	"github.com/matrix-org/go-neb/types"
	"github.com/matrix-org/go-neb/util"
	"net/http"
	"sort"
	"strings"
)

type githubWebhookService struct {
	id                 string
	serviceUserID      string
	webhookEndpointURL string
	ClientUserID       string // optional; required for webhooks
	RealmID            string
	SecretToken        string
	Rooms              map[string]struct { // room_id => {}
		Repos map[string]struct { // owner/repo => { events: ["push","issue","pull_request"] }
			Events []string
		}
	}
}

func (s *githubWebhookService) ServiceUserID() string { return s.serviceUserID }
func (s *githubWebhookService) ServiceID() string     { return s.id }
func (s *githubWebhookService) ServiceType() string   { return "github-webhook" }
func (s *githubWebhookService) Plugin(cli *matrix.Client, roomID string) plugin.Plugin {
	return plugin.Plugin{}
}
func (s *githubWebhookService) OnReceiveWebhook(w http.ResponseWriter, req *http.Request, cli *matrix.Client) {
	evType, repo, msg, err := webhook.OnReceiveRequest(req, s.SecretToken)
	if err != nil {
		w.WriteHeader(err.Code)
		return
	}
	logger := log.WithFields(log.Fields{
		"event": evType,
		"repo":  *repo.FullName,
	})
	repoExistsInConfig := false

	for roomID, roomConfig := range s.Rooms {
		for ownerRepo, repoConfig := range roomConfig.Repos {
			if !strings.EqualFold(*repo.FullName, ownerRepo) {
				continue
			}
			repoExistsInConfig = true // even if we don't notify for it.
			notifyRoom := false
			for _, notifyType := range repoConfig.Events {
				if evType == notifyType {
					notifyRoom = true
					break
				}
			}
			if notifyRoom {
				logger.WithFields(log.Fields{
					"msg":     msg,
					"room_id": roomID,
				}).Print("Sending notification to room")
				if _, e := cli.SendMessageEvent(roomID, "m.room.message", msg); e != nil {
					logger.WithError(e).WithField("room_id", roomID).Print(
						"Failed to send notification to room.")
				}
			}
		}
	}

	if !repoExistsInConfig {
		segs := strings.Split(*repo.FullName, "/")
		if len(segs) != 2 {
			logger.Error("Received event with malformed owner/repo.")
			w.WriteHeader(400)
			return
		}
		if err := s.deleteHook(segs[0], segs[1]); err != nil {
			logger.WithError(err).Print("Failed to delete webhook")
		} else {
			logger.Info("Deleted webhook")
		}
	}

	w.WriteHeader(200)
}

// Register will create webhooks for the repos specified in Rooms
//
// The hooks made are a delta between the old service and the current configuration. If all webhooks are made,
// Register() succeeds. If any webhook fails to be created, Register() fails. A delta is used to allow clients to incrementally
// build up the service config without recreating the hooks every time a change is made.
//
// Hooks are deleted when this service receives a webhook event from Github for a repo which has no user configurations.
//
// Hooks can get out of sync if a user manually deletes a hook in the Github UI. In this case, toggling the repo configuration will
// force NEB to recreate the hook.
func (s *githubWebhookService) Register(oldService types.Service, client *matrix.Client) error {
	if s.RealmID == "" || s.ClientUserID == "" {
		return fmt.Errorf("RealmID and ClientUserID is required")
	}
	realm, err := s.loadRealm()
	if err != nil {
		return err
	}

	// In order to register the GH service as a client, you must have authed with GH.
	cli := s.githubClientFor(s.ClientUserID, false)
	if cli == nil {
		return fmt.Errorf(
			"User %s does not have a Github auth session with realm %s.", s.ClientUserID, realm.ID())
	}

	// Fetch the old service list and work out the difference between the two services.
	var oldRepos []string
	if oldService != nil {
		old, ok := oldService.(*githubWebhookService)
		if !ok {
			log.WithFields(log.Fields{
				"service_id":   oldService.ServiceID(),
				"service_type": oldService.ServiceType(),
			}).Print("Cannot cast old github service to GithubWebhookService")
			// non-fatal though, we'll just make the hooks
		} else {
			oldRepos = old.repoList()
		}
	}

	reposForWebhooks := s.repoList()

	// Add hooks for the newly added repos but don't remove hooks for the removed repos: we'll clean those out later
	newRepos, removedRepos := util.Difference(reposForWebhooks, oldRepos)
	if len(reposForWebhooks) == 0 && len(removedRepos) == 0 {
		// The user didn't specify any webhooks. This may be a bug or it may be
		// a conscious decision to remove all webhooks for this service. Figure out
		// which it is by checking if we'd be removing any webhooks.
		return fmt.Errorf("No webhooks specified.")
	}
	for _, r := range newRepos {
		logger := log.WithField("repo", r)
		err := s.createHook(cli, r)
		if err != nil {
			logger.WithError(err).Error("Failed to create webhook")
			return err
		}
		logger.Info("Created webhook")
	}

	if err := s.joinWebhookRooms(client); err != nil {
		return err
	}

	log.Infof("%+v", s)

	return nil
}

func (s *githubWebhookService) PostRegister(oldService types.Service) {
	// Clean up removed repositories from the old service by working out the delta between
	// the old and new hooks.

	// Fetch the old service list
	var oldRepos []string
	if oldService != nil {
		old, ok := oldService.(*githubWebhookService)
		if !ok {
			log.WithFields(log.Fields{
				"service_id":   oldService.ServiceID(),
				"service_type": oldService.ServiceType(),
			}).Print("Cannot cast old github service to GithubWebhookService")
			return
		}
		oldRepos = old.repoList()
	}

	newRepos := s.repoList()

	// Register() handled adding the new repos, we just want to clean up after ourselves
	_, removedRepos := util.Difference(newRepos, oldRepos)
	for _, r := range removedRepos {
		segs := strings.Split(r, "/")
		if err := s.deleteHook(segs[0], segs[1]); err != nil {
			log.WithFields(log.Fields{
				log.ErrorKey: err,
				"repo":       r,
			}).Warn("Failed to remove webhook")
		}
	}

	// If we are not tracking any repos any more then we are back to square 1 and not doing anything
	// so remove ourselves from the database. This is safe because this is still within the critical
	// section for this service.
	if len(newRepos) == 0 {
		logger := log.WithFields(log.Fields{
			"service_type": s.ServiceType(),
			"service_id":   s.ServiceID(),
		})
		logger.Info("Removing service as no webhooks are registered.")
		if err := database.GetServiceDB().DeleteService(s.ServiceID()); err != nil {
			logger.WithError(err).Error("Failed to delete service")
		}
	}
}

func (s *githubWebhookService) joinWebhookRooms(client *matrix.Client) error {
	for roomID := range s.Rooms {
		if _, err := client.JoinRoom(roomID, "", ""); err != nil {
			// TODO: Leave the rooms we successfully joined?
			return err
		}
	}
	return nil
}

// Returns a list of "owner/repos"
func (s *githubWebhookService) repoList() []string {
	var repos []string
	if s.Rooms == nil {
		return repos
	}
	for _, roomConfig := range s.Rooms {
		for ownerRepo := range roomConfig.Repos {
			if strings.Count(ownerRepo, "/") != 1 {
				log.WithField("repo", ownerRepo).Error("Bad owner/repo key in config")
				continue
			}
			exists := false
			for _, r := range repos {
				if r == ownerRepo {
					exists = true
					break
				}
			}
			if !exists {
				repos = append(repos, ownerRepo)
			}
		}
	}
	return repos
}

func (s *githubWebhookService) createHook(cli *github.Client, ownerRepo string) error {
	o := strings.Split(ownerRepo, "/")
	owner := o[0]
	repo := o[1]
	// make a hook for all GH events since we'll filter it when we receive webhook requests
	name := "web" // https://developer.github.com/v3/repos/hooks/#create-a-hook
	cfg := map[string]interface{}{
		"content_type": "json",
		"url":          s.webhookEndpointURL,
	}
	if s.SecretToken != "" {
		cfg["secret"] = s.SecretToken
	}
	events := []string{"push", "pull_request", "issues", "issue_comment", "pull_request_review_comment"}
	_, res, err := cli.Repositories.CreateHook(owner, repo, &github.Hook{
		Name:   &name,
		Config: cfg,
		Events: events,
	})

	if res.StatusCode == 422 {
		errResponse, ok := err.(*github.ErrorResponse)
		if !ok {
			return err
		}
		for _, ghErr := range errResponse.Errors {
			if strings.Contains(ghErr.Message, "already exists") {
				log.WithField("repo", ownerRepo).Print("422 : Hook already exists")
				return nil
			}
		}
		return err
	}

	return err
}

func (s *githubWebhookService) deleteHook(owner, repo string) error {
	logger := log.WithFields(log.Fields{
		"endpoint": s.webhookEndpointURL,
		"repo":     owner + "/" + repo,
	})
	logger.Info("Removing hook")

	cli := s.githubClientFor(s.ClientUserID, false)
	if cli == nil {
		logger.WithField("user_id", s.ClientUserID).Print("Cannot delete webhook: no authenticated client exists for user ID.")
		return fmt.Errorf("no authenticated client exists for user ID")
	}

	// Get a list of webhooks for this owner/repo and find the one which has the
	// same endpoint URL which is what github uses to determine equivalence.
	hooks, _, err := cli.Repositories.ListHooks(owner, repo, nil)
	if err != nil {
		return err
	}
	var hook *github.Hook
	for _, h := range hooks {
		if h.Config["url"] == nil {
			logger.Print("Ignoring nil config.url")
			continue
		}
		hookURL, ok := h.Config["url"].(string)
		if !ok {
			logger.Print("Ignoring non-string config.url")
			continue
		}
		if hookURL == s.webhookEndpointURL {
			hook = h
			break
		}
	}
	if hook == nil {
		return fmt.Errorf("Failed to find hook with endpoint: %s", s.webhookEndpointURL)
	}

	_, err = cli.Repositories.DeleteHook(owner, repo, *hook.ID)
	return err
}

func sameRepos(a *githubWebhookService, b *githubWebhookService) bool {
	getRepos := func(s *githubWebhookService) []string {
		r := make(map[string]bool)
		for _, roomConfig := range s.Rooms {
			for ownerRepo := range roomConfig.Repos {
				r[ownerRepo] = true
			}
		}
		var rs []string
		for k := range r {
			rs = append(rs, k)
		}
		return rs
	}
	aRepos := getRepos(a)
	bRepos := getRepos(b)

	if len(aRepos) != len(bRepos) {
		return false
	}

	sort.Strings(aRepos)
	sort.Strings(bRepos)
	for i := 0; i < len(aRepos); i++ {
		if aRepos[i] != bRepos[i] {
			return false
		}
	}
	return true
}

func (s *githubWebhookService) githubClientFor(userID string, allowUnauth bool) *github.Client {
	token, err := getTokenForUser(s.RealmID, userID)
	if err != nil {
		log.WithFields(log.Fields{
			log.ErrorKey: err,
			"user_id":    userID,
			"realm_id":   s.RealmID,
		}).Print("Failed to get token for user")
	}
	if token != "" {
		return client.New(token)
	} else if allowUnauth {
		return client.New("")
	} else {
		return nil
	}
}

func (s *githubWebhookService) loadRealm() (types.AuthRealm, error) {
	if s.RealmID == "" {
		return nil, fmt.Errorf("Missing RealmID")
	}
	// check realm exists
	realm, err := database.GetServiceDB().LoadAuthRealm(s.RealmID)
	if err != nil {
		return nil, err
	}
	// make sure the realm is of the type we expect
	if realm.Type() != "github" {
		return nil, fmt.Errorf("Realm is of type '%s', not 'github'", realm.Type())
	}
	return realm, nil
}

func init() {
	types.RegisterService(func(serviceID, serviceUserID, webhookEndpointURL string) types.Service {
		return &githubWebhookService{
			id:                 serviceID,
			serviceUserID:      serviceUserID,
			webhookEndpointURL: webhookEndpointURL,
		}
	})
}
