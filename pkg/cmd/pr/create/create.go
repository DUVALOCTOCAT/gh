package create

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/api"
	"github.com/cli/cli/context"
	"github.com/cli/cli/git"
	"github.com/cli/cli/internal/config"
	"github.com/cli/cli/internal/ghrepo"
	"github.com/cli/cli/pkg/cmd/pr/shared"
	"github.com/cli/cli/pkg/cmdutil"
	"github.com/cli/cli/pkg/cmdutil/action"
	"github.com/cli/cli/pkg/iostreams"
	"github.com/cli/cli/pkg/prompt"
	"github.com/cli/cli/utils"
	"github.com/rsteube/carapace"
	"github.com/spf13/cobra"
)

type CreateOptions struct {
	// This struct stores user input and factory functions
	HttpClient func() (*http.Client, error)
	Config     func() (config.Config, error)
	IO         *iostreams.IOStreams
	Remotes    func() (context.Remotes, error)
	Branch     func() (string, error)

	TitleProvided bool
	BodyProvided  bool

	RootDirOverride string
	RepoOverride    string

	Autofill    bool
	WebMode     bool
	RecoverFile string

	IsDraft    bool
	Title      string
	Body       string
	BaseBranch string
	HeadBranch string

	Reviewers []string
	Assignees []string
	Labels    []string
	Projects  []string
	Milestone string
}

type CreateContext struct {
	// This struct stores contextual data about the creation process and is for building up enough
	// data to create a pull request
	RepoContext        *context.ResolvedRemotes
	BaseRepo           ghrepo.Interface
	HeadRepo           ghrepo.Interface
	BaseTrackingBranch string
	BaseBranch         string
	HeadBranch         string
	HeadBranchLabel    string
	HeadRemote         *context.Remote
	IsPushEnabled      bool
	Client             *api.Client
}

func NewCmdCreate(f *cmdutil.Factory, runF func(*CreateOptions) error) *cobra.Command {
	opts := &CreateOptions{
		IO:         f.IOStreams,
		HttpClient: f.HttpClient,
		Config:     f.Config,
		Remotes:    f.Remotes,
		Branch:     f.Branch,
	}

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a pull request",
		Long: heredoc.Doc(`
			Create a pull request on GitHub.

			When the current branch isn't fully pushed to a git remote, a prompt will ask where
			to push the branch and offer an option to fork the base repository. Use '--head' to
			explicitly skip any forking or pushing behavior.

			A prompt will also ask for the title and the body of the pull request. Use '--title'
			and '--body' to skip this, or use '--fill' to autofill these values from git commits.
		`),
		Example: heredoc.Doc(`
			$ gh pr create --title "The bug is fixed" --body "Everything works again"
			$ gh pr create --reviewer monalisa,hubot
			$ gh pr create --project "Roadmap"
			$ gh pr create --base develop --head monalisa:feature
		`),
		Args: cmdutil.NoArgsQuoteReminder,
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.TitleProvided = cmd.Flags().Changed("title")
			opts.BodyProvided = cmd.Flags().Changed("body")
			opts.RepoOverride, _ = cmd.Flags().GetString("repo")

			if !opts.IO.CanPrompt() && opts.RecoverFile != "" {
				return &cmdutil.FlagError{Err: errors.New("--recover only supported when running interactively")}
			}

			if !opts.IO.CanPrompt() && !opts.WebMode && !opts.TitleProvided && !opts.Autofill {
				return &cmdutil.FlagError{Err: errors.New("--title or --fill required when not running interactively")}
			}

			if opts.IsDraft && opts.WebMode {
				return errors.New("the --draft flag is not supported with --web")
			}
			if len(opts.Reviewers) > 0 && opts.WebMode {
				return errors.New("the --reviewer flag is not supported with --web")
			}

			if runF != nil {
				return runF(opts)
			}
			return createRun(opts)
		},
	}

	fl := cmd.Flags()
	fl.BoolVarP(&opts.IsDraft, "draft", "d", false, "Mark pull request as a draft")
	fl.StringVarP(&opts.Title, "title", "t", "", "Title for the pull request")
	fl.StringVarP(&opts.Body, "body", "b", "", "Body for the pull request")
	fl.StringVarP(&opts.BaseBranch, "base", "B", "", "The `branch` into which you want your code merged")
	fl.StringVarP(&opts.HeadBranch, "head", "H", "", "The `branch` that contains commits for your pull request (default: current branch)")
	fl.BoolVarP(&opts.WebMode, "web", "w", false, "Open the web browser to create a pull request")
	fl.BoolVarP(&opts.Autofill, "fill", "f", false, "Do not prompt for title/body and just use commit info")
	fl.StringSliceVarP(&opts.Reviewers, "reviewer", "r", nil, "Request reviews from people by their `login`")
	fl.StringSliceVarP(&opts.Assignees, "assignee", "a", nil, "Assign people by their `login`")
	fl.StringSliceVarP(&opts.Labels, "label", "l", nil, "Add labels by `name`")
	fl.StringSliceVarP(&opts.Projects, "project", "p", nil, "Add the pull request to projects by `name`")
	fl.StringVarP(&opts.Milestone, "milestone", "m", "", "Add the pull request to a milestone by `name`")
	fl.StringVar(&opts.RecoverFile, "recover", "", "Recover input from a failed run of create")

	cmdutil.DeferCompletion(func() {
		carapace.Gen(cmd).FlagCompletion(carapace.ActionMap{
			"assignee": carapace.ActionMultiParts(",", func(args, parts []string) carapace.Action {
				return action.ActionAssignableUsers(cmd).Invoke(args).Filter(parts).ToA()
			}),
			"base": action.ActionBranches(cmd),
			"head": action.ActionBranches(cmd),
			"label": carapace.ActionMultiParts(",", func(args, parts []string) carapace.Action {
				return action.ActionLabels(cmd).Invoke(args).Filter(parts).ToA()
			}),
			"milestone": action.ActionMilestones(cmd),
			// TODO "project": ActionProjects(),
			"reviewer": carapace.ActionMultiParts(",", func(args, parts []string) carapace.Action {
				return action.ActionAssignableUsers(cmd).Invoke(args).Filter(parts).ToA()
			}),
		})
	})

	return cmd
}

func createRun(opts *CreateOptions) (err error) {
	ctx, err := NewCreateContext(opts)
	if err != nil {
		return
	}

	client := ctx.Client

	state, err := NewIssueState(*ctx, *opts)
	if err != nil {
		return
	}

	if opts.WebMode {
		if !opts.Autofill {
			state.Title = opts.Title
			state.Body = opts.Body
		}
		err = handlePush(*opts, *ctx)
		if err != nil {
			return
		}
		return previewPR(*opts, *ctx, *state)
	}

	if opts.TitleProvided {
		state.Title = opts.Title
	}

	if opts.BodyProvided {
		state.Body = opts.Body
	}

	existingPR, err := api.PullRequestForBranch(
		client, ctx.BaseRepo, ctx.BaseBranch, ctx.HeadBranchLabel, []string{"OPEN"})
	var notFound *api.NotFoundError
	if err != nil && !errors.As(err, &notFound) {
		return fmt.Errorf("error checking for existing pull request: %w", err)
	}
	if err == nil {
		return fmt.Errorf("a pull request for branch %q into branch %q already exists:\n%s",
			ctx.HeadBranchLabel, ctx.BaseBranch, existingPR.URL)
	}

	message := "\nCreating pull request for %s into %s in %s\n\n"
	if state.Draft {
		message = "\nCreating draft pull request for %s into %s in %s\n\n"
	}

	cs := opts.IO.ColorScheme()

	if opts.IO.CanPrompt() {
		fmt.Fprintf(opts.IO.ErrOut, message,
			cs.Cyan(ctx.HeadBranchLabel),
			cs.Cyan(ctx.BaseBranch),
			ghrepo.FullName(ctx.BaseRepo))
	}

	if opts.Autofill || (opts.TitleProvided && opts.BodyProvided) {
		err = handlePush(*opts, *ctx)
		if err != nil {
			return
		}
		return submitPR(*opts, *ctx, *state)
	}

	if opts.RecoverFile != "" {
		err = shared.FillFromJSON(opts.IO, opts.RecoverFile, state)
		if err != nil {
			return fmt.Errorf("failed to recover input: %w", err)
		}
	}

	if !opts.TitleProvided {
		err = shared.TitleSurvey(state)
		if err != nil {
			return
		}
	}

	editorCommand, err := cmdutil.DetermineEditor(opts.Config)
	if err != nil {
		return
	}

	defer shared.PreserveInput(opts.IO, state, &err)()

	templateContent := ""
	if !opts.BodyProvided {
		if opts.RecoverFile == "" {
			templateFiles, legacyTemplate := shared.FindTemplates(opts.RootDirOverride, "PULL_REQUEST_TEMPLATE")

			templateContent, err = shared.TemplateSurvey(templateFiles, legacyTemplate, *state)
			if err != nil {
				return
			}
		}

		err = shared.BodySurvey(state, templateContent, editorCommand)
		if err != nil {
			return
		}

		if state.Body == "" {
			state.Body = templateContent
		}
	}

	allowMetadata := ctx.BaseRepo.(*api.Repository).ViewerCanTriage()
	action, err := shared.ConfirmSubmission(!state.HasMetadata(), allowMetadata)
	if err != nil {
		return fmt.Errorf("unable to confirm: %w", err)
	}

	if action == shared.MetadataAction {
		err = shared.MetadataSurvey(opts.IO, client, ctx.BaseRepo, state)
		if err != nil {
			return
		}

		action, err = shared.ConfirmSubmission(!state.HasMetadata(), false)
		if err != nil {
			return
		}
	}

	if action == shared.CancelAction {
		fmt.Fprintln(opts.IO.ErrOut, "Discarding.")
		return nil
	}

	err = handlePush(*opts, *ctx)
	if err != nil {
		return
	}

	if action == shared.PreviewAction {
		return previewPR(*opts, *ctx, *state)
	}

	if action == shared.SubmitAction {
		return submitPR(*opts, *ctx, *state)
	}

	err = errors.New("expected to cancel, preview, or submit")
	return
}

func initDefaultTitleBody(ctx CreateContext, state *shared.IssueMetadataState) error {
	baseRef := ctx.BaseTrackingBranch
	headRef := ctx.HeadBranch

	commits, err := git.Commits(baseRef, headRef)
	if err != nil {
		return err
	}

	if len(commits) == 1 {
		state.Title = commits[0].Title
		body, err := git.CommitBody(commits[0].Sha)
		if err != nil {
			return err
		}
		state.Body = body
	} else {
		state.Title = utils.Humanize(headRef)

		var body strings.Builder
		for i := len(commits) - 1; i >= 0; i-- {
			fmt.Fprintf(&body, "- %s\n", commits[i].Title)
		}
		state.Body = body.String()
	}

	return nil
}

func determineTrackingBranch(remotes context.Remotes, headBranch string) *git.TrackingRef {
	refsForLookup := []string{"HEAD"}
	var trackingRefs []git.TrackingRef

	headBranchConfig := git.ReadBranchConfig(headBranch)
	if headBranchConfig.RemoteName != "" {
		tr := git.TrackingRef{
			RemoteName: headBranchConfig.RemoteName,
			BranchName: strings.TrimPrefix(headBranchConfig.MergeRef, "refs/heads/"),
		}
		trackingRefs = append(trackingRefs, tr)
		refsForLookup = append(refsForLookup, tr.String())
	}

	for _, remote := range remotes {
		tr := git.TrackingRef{
			RemoteName: remote.Name,
			BranchName: headBranch,
		}
		trackingRefs = append(trackingRefs, tr)
		refsForLookup = append(refsForLookup, tr.String())
	}

	resolvedRefs, _ := git.ShowRefs(refsForLookup...)
	if len(resolvedRefs) > 1 {
		for _, r := range resolvedRefs[1:] {
			if r.Hash != resolvedRefs[0].Hash {
				continue
			}
			for _, tr := range trackingRefs {
				if tr.String() != r.Name {
					continue
				}
				return &tr
			}
		}
	}

	return nil
}

func NewIssueState(ctx CreateContext, opts CreateOptions) (*shared.IssueMetadataState, error) {
	var milestoneTitles []string
	if opts.Milestone != "" {
		milestoneTitles = []string{opts.Milestone}
	}

	state := &shared.IssueMetadataState{
		Type:       shared.PRMetadata,
		Reviewers:  opts.Reviewers,
		Assignees:  opts.Assignees,
		Labels:     opts.Labels,
		Projects:   opts.Projects,
		Milestones: milestoneTitles,
		Draft:      opts.IsDraft,
	}

	if opts.Autofill || !opts.TitleProvided || !opts.BodyProvided {
		err := initDefaultTitleBody(ctx, state)
		if err != nil {
			return nil, fmt.Errorf("could not compute title or body defaults: %w", err)
		}
	}

	return state, nil
}

func NewCreateContext(opts *CreateOptions) (*CreateContext, error) {
	httpClient, err := opts.HttpClient()
	if err != nil {
		return nil, err
	}
	client := api.NewClientFromHTTP(httpClient)

	remotes, err := opts.Remotes()
	if err != nil {
		return nil, err
	}

	repoContext, err := context.ResolveRemotesToRepos(remotes, client, opts.RepoOverride)
	if err != nil {
		return nil, err
	}

	var baseRepo *api.Repository
	if br, err := repoContext.BaseRepo(opts.IO); err == nil {
		if r, ok := br.(*api.Repository); ok {
			baseRepo = r
		} else {
			// TODO: if RepoNetwork is going to be requested anyway in `repoContext.HeadRepos()`,
			// consider piggybacking on that result instead of performing a separate lookup
			baseRepo, err = api.GitHubRepo(client, br)
			if err != nil {
				return nil, err
			}
		}
	} else {
		return nil, fmt.Errorf("could not determine base repository: %w", err)
	}

	isPushEnabled := false
	headBranch := opts.HeadBranch
	headBranchLabel := opts.HeadBranch
	if headBranch == "" {
		headBranch, err = opts.Branch()
		if err != nil {
			return nil, fmt.Errorf("could not determine the current branch: %w", err)
		}
		headBranchLabel = headBranch
		isPushEnabled = true
	} else if idx := strings.IndexRune(headBranch, ':'); idx >= 0 {
		headBranch = headBranch[idx+1:]
	}

	if ucc, err := git.UncommittedChangeCount(); err == nil && ucc > 0 {
		fmt.Fprintf(opts.IO.ErrOut, "Warning: %s\n", utils.Pluralize(ucc, "uncommitted change"))
	}

	var headRepo ghrepo.Interface
	var headRemote *context.Remote

	if isPushEnabled {
		// determine whether the head branch is already pushed to a remote
		if pushedTo := determineTrackingBranch(remotes, headBranch); pushedTo != nil {
			isPushEnabled = false
			if r, err := remotes.FindByName(pushedTo.RemoteName); err == nil {
				headRepo = r
				headRemote = r
				headBranchLabel = pushedTo.BranchName
				if !ghrepo.IsSame(baseRepo, headRepo) {
					headBranchLabel = fmt.Sprintf("%s:%s", headRepo.RepoOwner(), pushedTo.BranchName)
				}
			}
		}
	}

	// otherwise, ask the user for the head repository using info obtained from the API
	if headRepo == nil && isPushEnabled && opts.IO.CanPrompt() {
		pushableRepos, err := repoContext.HeadRepos()
		if err != nil {
			return nil, err
		}

		if len(pushableRepos) == 0 {
			pushableRepos, err = api.RepoFindForks(client, baseRepo, 3)
			if err != nil {
				return nil, err
			}
		}

		currentLogin, err := api.CurrentLoginName(client, baseRepo.RepoHost())
		if err != nil {
			return nil, err
		}

		hasOwnFork := false
		var pushOptions []string
		for _, r := range pushableRepos {
			pushOptions = append(pushOptions, ghrepo.FullName(r))
			if r.RepoOwner() == currentLogin {
				hasOwnFork = true
			}
		}

		if !hasOwnFork {
			pushOptions = append(pushOptions, "Create a fork of "+ghrepo.FullName(baseRepo))
		}
		pushOptions = append(pushOptions, "Skip pushing the branch")
		pushOptions = append(pushOptions, "Cancel")

		var selectedOption int
		err = prompt.SurveyAskOne(&survey.Select{
			Message: fmt.Sprintf("Where should we push the '%s' branch?", headBranch),
			Options: pushOptions,
		}, &selectedOption)
		if err != nil {
			return nil, err
		}

		if selectedOption < len(pushableRepos) {
			headRepo = pushableRepos[selectedOption]
			if !ghrepo.IsSame(baseRepo, headRepo) {
				headBranchLabel = fmt.Sprintf("%s:%s", headRepo.RepoOwner(), headBranch)
			}
		} else if pushOptions[selectedOption] == "Skip pushing the branch" {
			isPushEnabled = false
		} else if pushOptions[selectedOption] == "Cancel" {
			return nil, cmdutil.SilentError
		} else {
			// "Create a fork of ..."
			if baseRepo.IsPrivate {
				return nil, fmt.Errorf("cannot fork private repository %s", ghrepo.FullName(baseRepo))
			}
			headBranchLabel = fmt.Sprintf("%s:%s", currentLogin, headBranch)
		}
	}

	if headRepo == nil && isPushEnabled && !opts.IO.CanPrompt() {
		fmt.Fprintf(opts.IO.ErrOut, "aborted: you must first push the current branch to a remote, or use the --head flag")
		return nil, cmdutil.SilentError
	}

	baseBranch := opts.BaseBranch
	if baseBranch == "" {
		baseBranch = baseRepo.DefaultBranchRef.Name
	}
	if headBranch == baseBranch && headRepo != nil && ghrepo.IsSame(baseRepo, headRepo) {
		return nil, fmt.Errorf("must be on a branch named differently than %q", baseBranch)
	}

	baseTrackingBranch := baseBranch
	if baseRemote, err := remotes.FindByRepo(baseRepo.RepoOwner(), baseRepo.RepoName()); err == nil {
		baseTrackingBranch = fmt.Sprintf("%s/%s", baseRemote.Name, baseBranch)
	}

	return &CreateContext{
		BaseRepo:           baseRepo,
		HeadRepo:           headRepo,
		BaseBranch:         baseBranch,
		BaseTrackingBranch: baseTrackingBranch,
		HeadBranch:         headBranch,
		HeadBranchLabel:    headBranchLabel,
		HeadRemote:         headRemote,
		IsPushEnabled:      isPushEnabled,
		RepoContext:        repoContext,
		Client:             client,
	}, nil

}

func submitPR(opts CreateOptions, ctx CreateContext, state shared.IssueMetadataState) error {
	client := ctx.Client

	params := map[string]interface{}{
		"title":       state.Title,
		"body":        state.Body,
		"draft":       state.Draft,
		"baseRefName": ctx.BaseBranch,
		"headRefName": ctx.HeadBranchLabel,
	}

	if params["title"] == "" {
		return errors.New("pull request title must not be blank")
	}

	err := shared.AddMetadataToIssueParams(client, ctx.BaseRepo, params, &state)
	if err != nil {
		return err
	}

	pr, err := api.CreatePullRequest(client, ctx.BaseRepo.(*api.Repository), params)
	if pr != nil {
		fmt.Fprintln(opts.IO.Out, pr.URL)
	}
	if err != nil {
		if pr != nil {
			return fmt.Errorf("pull request update failed: %w", err)
		}
		return fmt.Errorf("pull request create failed: %w", err)
	}
	return nil
}

func previewPR(opts CreateOptions, ctx CreateContext, state shared.IssueMetadataState) error {
	openURL, err := generateCompareURL(ctx, state)
	if err != nil {
		return err
	}

	if opts.IO.IsStdinTTY() && opts.IO.IsStdoutTTY() {
		fmt.Fprintf(opts.IO.ErrOut, "Opening %s in your browser.\n", utils.DisplayURL(openURL))
	}
	return utils.OpenInBrowser(openURL)

}

func handlePush(opts CreateOptions, ctx CreateContext) error {
	didForkRepo := false
	headRepo := ctx.HeadRepo
	headRemote := ctx.HeadRemote
	client := ctx.Client

	var err error
	// if a head repository could not be determined so far, automatically create
	// one by forking the base repository
	if headRepo == nil && ctx.IsPushEnabled {
		headRepo, err = api.ForkRepo(client, ctx.BaseRepo)
		if err != nil {
			return fmt.Errorf("error forking repo: %w", err)
		}
		didForkRepo = true
	}

	if headRemote == nil && headRepo != nil {
		headRemote, _ = ctx.RepoContext.RemoteForRepo(headRepo)
	}

	// There are two cases when an existing remote for the head repo will be
	// missing:
	// 1. the head repo was just created by auto-forking;
	// 2. an existing fork was discovered by querying the API.
	//
	// In either case, we want to add the head repo as a new git remote so we
	// can push to it.
	if headRemote == nil && ctx.IsPushEnabled {
		cfg, err := opts.Config()
		if err != nil {
			return err
		}
		cloneProtocol, _ := cfg.Get(headRepo.RepoHost(), "git_protocol")

		headRepoURL := ghrepo.FormatRemoteURL(headRepo, cloneProtocol)

		// TODO: prevent clashes with another remote of a same name
		gitRemote, err := git.AddRemote("fork", headRepoURL)
		if err != nil {
			return fmt.Errorf("error adding remote: %w", err)
		}
		headRemote = &context.Remote{
			Remote: gitRemote,
			Repo:   headRepo,
		}
	}

	// automatically push the branch if it hasn't been pushed anywhere yet
	if ctx.IsPushEnabled {
		pushBranch := func() error {
			pushTries := 0
			maxPushTries := 3
			for {
				r := NewRegexpWriter(opts.IO.ErrOut, gitPushRegexp, "")
				defer r.Flush()
				cmdErr := r
				cmdOut := opts.IO.Out
				if err := git.Push(headRemote.Name, fmt.Sprintf("HEAD:%s", ctx.HeadBranch), cmdOut, cmdErr); err != nil {
					if didForkRepo && pushTries < maxPushTries {
						pushTries++
						// first wait 2 seconds after forking, then 4s, then 6s
						waitSeconds := 2 * pushTries
						fmt.Fprintf(opts.IO.ErrOut, "waiting %s before retrying...\n", utils.Pluralize(waitSeconds, "second"))
						time.Sleep(time.Duration(waitSeconds) * time.Second)
						continue
					}
					return err
				}
				break
			}
			return nil
		}

		err := pushBranch()
		if err != nil {
			return err
		}
	}

	return nil
}

func generateCompareURL(ctx CreateContext, state shared.IssueMetadataState) (string, error) {
	u := ghrepo.GenerateRepoURL(
		ctx.BaseRepo,
		"compare/%s...%s?expand=1",
		url.QueryEscape(ctx.BaseBranch), url.QueryEscape(ctx.HeadBranch))
	url, err := shared.WithPrAndIssueQueryParams(u, state)
	if err != nil {
		return "", err
	}
	return url, nil
}

var gitPushRegexp = regexp.MustCompile("^remote: (Create a pull request.*by visiting|[[:space:]]*https://.*/pull/new/).*\n?$")
