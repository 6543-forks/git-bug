package gitea

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"code.gitea.io/sdk/gitea"
	"github.com/pkg/errors"

	"github.com/MichaelMure/git-bug/bridge/core"
	"github.com/MichaelMure/git-bug/bridge/core/auth"
	"github.com/MichaelMure/git-bug/bug"
	"github.com/MichaelMure/git-bug/cache"
	"github.com/MichaelMure/git-bug/entity"
	"github.com/MichaelMure/git-bug/identity"
)

var (
	ErrMissingIdentityToken = errors.New("missing identity token")
)

// giteaExporter implement the Exporter interface
type giteaExporter struct {
	conf core.Configuration

	// cache identities clients
	identityClient map[entity.Id]*gitea.Client

	// gitea repository Owner
	repositoryOwner string
	// gitea repository Name
	repositoryName string

	// cache identifiers used to speed up exporting operations
	// cleared for each bug
	cachedOperationIDs map[string]string
}

// Init .
func (ge *giteaExporter) Init(ctx context.Context, repo *cache.RepoCache, conf core.Configuration) error {
	ge.conf = conf
	ge.identityClient = make(map[entity.Id]*gitea.Client)
	ge.cachedOperationIDs = make(map[string]string)

	// get repository owner
	ge.repositoryOwner = ge.conf[confKeyOwner]
	// get repository name
	ge.repositoryName = ge.conf[confKeyProject]

	// preload all clients
	err := ge.cacheAllClient(ctx, repo, ge.conf[confKeyGiteaBaseUrl])
	if err != nil {
		return err
	}

	return nil
}

func (ge *giteaExporter) cacheAllClient(ctx context.Context, repo *cache.RepoCache, baseURL string) error {
	creds, err := auth.List(repo,
		auth.WithTarget(target),
		auth.WithKind(auth.KindToken),
		auth.WithMeta(auth.MetaKeyBaseURL, baseURL),
	)
	if err != nil {
		return err
	}

	for _, cred := range creds {
		login, ok := cred.GetMetadata(auth.MetaKeyLogin)
		if !ok {
			_, _ = fmt.Fprintf(os.Stderr, "credential %s is not tagged with a Gitea login\n", cred.ID().Human())
			continue
		}

		user, err := repo.ResolveIdentityImmutableMetadata(metaKeyGiteaLogin, login)
		if err == identity.ErrIdentityNotExist {
			continue
		}
		if err != nil {
			return nil
		}

		if _, ok := ge.identityClient[user.Id()]; !ok {
			client, err := buildClient(ge.conf[confKeyGiteaBaseUrl], creds[0].(*auth.Token))
			if err != nil {
				return err
			}
			ge.identityClient[user.Id()] = client
		}
	}

	return nil
}

// getIdentityClient return a gitea API client configured with the access token of the given identity.
func (ge *giteaExporter) getIdentityClient(userId entity.Id) (*gitea.Client, error) {
	client, ok := ge.identityClient[userId]
	if ok {
		return client, nil
	}

	return nil, ErrMissingIdentityToken
}

// ExportAll export all event made by the current user to Gitea
func (ge *giteaExporter) ExportAll(ctx context.Context, repo *cache.RepoCache, since time.Time) (<-chan core.ExportResult, error) {
	out := make(chan core.ExportResult)

	go func() {
		defer close(out)

		allIdentitiesIds := make([]entity.Id, 0, len(ge.identityClient))
		for id := range ge.identityClient {
			allIdentitiesIds = append(allIdentitiesIds, id)
		}

		allBugsIds := repo.AllBugsIds()

		for _, id := range allBugsIds {
			select {
			case <-ctx.Done():
				return
			default:
				b, err := repo.ResolveBug(id)
				if err != nil {
					out <- core.NewExportError(err, id)
					return
				}

				snapshot := b.Snapshot()

				// ignore issues created before since date
				// TODO: compare the Lamport time instead of using the unix time
				if snapshot.CreateTime.Before(since) {
					out <- core.NewExportNothing(b.Id(), "bug created before the since date")
					continue
				}

				if snapshot.HasAnyActor(allIdentitiesIds...) {
					// try to export the bug and it associated events
					ge.exportBug(ctx, b, out)
				}
			}
		}
	}()

	return out, nil
}

// exportBug publish bugs and related events
func (ge *giteaExporter) exportBug(ctx context.Context, b *cache.BugCache, out chan<- core.ExportResult) {
	snapshot := b.Snapshot()

	var bugUpdated bool
	var err error
	var bugGiteaID int
	var bugGiteaIDString string
	var GiteaBaseUrl string
	var bugCreationId string

	// Special case:
	// if a user try to export a bug that is not already exported to Gitea (or imported
	// from Gitea) and we do not have the token of the bug author, there is nothing we can do.

	// skip bug if origin is not allowed
	origin, ok := snapshot.GetCreateMetadata(core.MetaKeyOrigin)
	if ok && origin != target {
		out <- core.NewExportNothing(b.Id(), fmt.Sprintf("issue tagged with origin: %s", origin))
		return
	}

	// first operation is always createOp
	createOp := snapshot.Operations[0].(*bug.CreateOperation)
	author := snapshot.Author

	// get gitea bug ID
	giteaID, ok := snapshot.GetCreateMetadata(metaKeyGiteaId)
	if ok {
		giteaBaseUrl, ok := snapshot.GetCreateMetadata(metaKeyGiteaBaseUrl)
		if ok && giteaBaseUrl != ge.conf[confKeyGiteaBaseUrl] {
			out <- core.NewExportNothing(b.Id(), "skipping issue imported from another Gitea instance")
			return
		}

		projectID, ok := snapshot.GetCreateMetadata(metaKeyGiteaProject)
		if !ok {
			err := fmt.Errorf("expected to find gitea project id")
			out <- core.NewExportError(err, b.Id())
			return
		}

		if projectID != ge.conf[confKeyProjectID] {
			out <- core.NewExportNothing(b.Id(), "skipping issue imported from another repository")
			return
		}

		// will be used to mark operation related to a bug as exported
		bugGiteaIDString = giteaID
		bugGiteaID, err = strconv.Atoi(bugGiteaIDString)
		if err != nil {
			out <- core.NewExportError(fmt.Errorf("unexpected gitea id format: %s", bugGiteaIDString), b.Id())
			return
		}

	} else {
		// check that we have a token for operation author
		client, err := ge.getIdentityClient(author.Id())
		if err != nil {
			// if bug is still not exported and we do not have the author stop the execution
			out <- core.NewExportNothing(b.Id(), fmt.Sprintf("missing author token"))
			return
		}

		// create bug
		_, id, url, err := createGiteaIssue(ctx, client, ge.repositoryID, createOp.Title, createOp.Message)
		if err != nil {
			err := errors.Wrap(err, "exporting gitea issue")
			out <- core.NewExportError(err, b.Id())
			return
		}

		idString := strconv.Itoa(id)
		out <- core.NewExportBug(b.Id())

		_, err = b.SetMetadata(
			createOp.Id(),
			map[string]string{
				metaKeyGiteaId:      idString,
				metaKeyGiteaUrl:     url,
				metaKeyGiteaBaseUrl: GiteaBaseUrl,
			},
		)
		if err != nil {
			err := errors.Wrap(err, "marking operation as exported")
			out <- core.NewExportError(err, b.Id())
			return
		}

		// commit operation to avoid creating multiple issues with multiple pushes
		if err := b.CommitAsNeeded(); err != nil {
			err := errors.Wrap(err, "bug commit")
			out <- core.NewExportError(err, b.Id())
			return
		}

		// cache bug gitea ID and URL
		bugGiteaID = id
		bugGiteaIDString = idString
	}

	bugCreationId = createOp.Id().String()
	// cache operation gitea id
	ge.cachedOperationIDs[bugCreationId] = bugGiteaIDString

	labelSet := make(map[string]struct{})
	for _, op := range snapshot.Operations[1:] {
		// ignore SetMetadata operations
		if _, ok := op.(*bug.SetMetadataOperation); ok {
			continue
		}

		// ignore operations already existing in gitea (due to import or export)
		// cache the ID of already exported or imported issues and events from Gitea
		if id, ok := op.GetMetadata(metaKeyGiteaId); ok {
			ge.cachedOperationIDs[op.Id().String()] = id
			continue
		}

		opAuthor := op.GetAuthor()
		client, err := ge.getIdentityClient(opAuthor.Id())
		if err != nil {
			continue
		}

		var id int
		var idString, url string
		switch op := op.(type) {
		case *bug.AddCommentOperation:

			// send operation to gitea
			id, err = addCommentGiteaIssue(ctx, client, ge.repositoryID, bugGiteaID, op.Message)
			if err != nil {
				err := errors.Wrap(err, "adding comment")
				out <- core.NewExportError(err, b.Id())
				return
			}

			out <- core.NewExportComment(op.Id())

			idString = strconv.Itoa(id)
			// cache comment id
			ge.cachedOperationIDs[op.Id().String()] = idString

		case *bug.EditCommentOperation:
			targetId := op.Target.String()

			// Since gitea doesn't consider the issue body as a comment
			if targetId == bugCreationId {

				// case bug creation operation: we need to edit the Gitea issue
				if err := updateGiteaIssueBody(ctx, client, ge.repositoryID, bugGiteaID, op.Message); err != nil {
					err := errors.Wrap(err, "editing issue")
					out <- core.NewExportError(err, b.Id())
					return
				}

				out <- core.NewExportCommentEdition(op.Id())
				id = bugGiteaID

			} else {

				// case comment edition operation: we need to edit the Gitea comment
				commentID, ok := ge.cachedOperationIDs[targetId]
				if !ok {
					out <- core.NewExportError(fmt.Errorf("unexpected error: comment id not found"), op.Target)
					return
				}

				commentIDint, err := strconv.Atoi(commentID)
				if err != nil {
					out <- core.NewExportError(fmt.Errorf("unexpected comment id format"), op.Target)
					return
				}

				if err := editCommentGiteaIssue(ctx, client, ge.repositoryID, bugGiteaID, commentIDint, op.Message); err != nil {
					err := errors.Wrap(err, "editing comment")
					out <- core.NewExportError(err, b.Id())
					return
				}

				out <- core.NewExportCommentEdition(op.Id())
				id = commentIDint
			}

		case *bug.SetStatusOperation:
			if err := updateGiteaIssueStatus(ctx, client, ge.repositoryID, bugGiteaID, op.Status); err != nil {
				err := errors.Wrap(err, "editing status")
				out <- core.NewExportError(err, b.Id())
				return
			}

			out <- core.NewExportStatusChange(op.Id())
			id = bugGiteaID

		case *bug.SetTitleOperation:
			if err := updateGiteaIssueTitle(ctx, client, ge.repositoryID, bugGiteaID, op.Title); err != nil {
				err := errors.Wrap(err, "editing title")
				out <- core.NewExportError(err, b.Id())
				return
			}

			out <- core.NewExportTitleEdition(op.Id())
			id = bugGiteaID

		case *bug.LabelChangeOperation:
			// we need to set the actual list of labels at each label change operation
			// because gitea update issue requests need directly the latest list of the verison

			for _, label := range op.Added {
				labelSet[label.String()] = struct{}{}
			}

			for _, label := range op.Removed {
				delete(labelSet, label.String())
			}

			labels := make([]string, 0, len(labelSet))
			for key := range labelSet {
				labels = append(labels, key)
			}

			if err := updateGiteaIssueLabels(ctx, client, ge.repositoryID, bugGiteaID, labels); err != nil {
				err := errors.Wrap(err, "updating labels")
				out <- core.NewExportError(err, b.Id())
				return
			}

			out <- core.NewExportLabelChange(op.Id())
			id = bugGiteaID
		default:
			panic("unhandled operation type case")
		}

		idString = strconv.Itoa(id)
		// mark operation as exported
		if err := markOperationAsExported(b, op.Id(), idString, url); err != nil {
			err := errors.Wrap(err, "marking operation as exported")
			out <- core.NewExportError(err, b.Id())
			return
		}

		// commit at each operation export to avoid exporting same events multiple times
		if err := b.CommitAsNeeded(); err != nil {
			err := errors.Wrap(err, "bug commit")
			out <- core.NewExportError(err, b.Id())
			return
		}

		bugUpdated = true
	}

	if !bugUpdated {
		out <- core.NewExportNothing(b.Id(), "nothing has been exported")
	}
}

func markOperationAsExported(b *cache.BugCache, target entity.Id, giteaID, giteaURL string) error {
	_, err := b.SetMetadata(
		target,
		map[string]string{
			metaKeyGiteaId:  giteaID,
			metaKeyGiteaUrl: giteaURL,
		},
	)

	return err
}

// create a gitea. issue and return it ID
func createGiteaIssue(ctx context.Context, gc *gitea.Client, rOwner, rName, title, body string) (int, int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	gc.SetContext(ctx)
	defer cancel()
	issue, _, err := gc.CreateIssue(rOwner, rName,
		gitea.CreateIssueOption{
			Title: title,
			Body:  body,
		},
	)
	if err != nil {
		return 0, 0, "", err
	}

	return issue.ID, issue.IID, issue.URL, nil
}

// add a comment to an issue and return it ID
func addCommentGiteaIssue(ctx context.Context, gc *gitea.Client, repositoryID string, issueID int, body string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	gc.SetContext(ctx)
	defer cancel()
	note, _, err := gc.Notes.CreateIssueNote(
		repositoryID, issueID,
		&gitea.CreateIssueNoteOptions{
			Body: &body,
		},
		gitea.WithContext(ctx),
	)
	if err != nil {
		return 0, err
	}

	return note.ID, nil
}

func editCommentGiteaIssue(ctx context.Context, gc *gitea.Client, repositoryID string, issueID, noteID int, body string) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	gc.SetContext(ctx)
	defer cancel()
	_, _, err := gc.Notes.UpdateIssueNote(
		repositoryID, issueID, noteID,
		&gitea.UpdateIssueNoteOptions{
			Body: &body,
		},
		gitea.WithContext(ctx),
	)

	return err
}

func updateGiteaIssueStatus(ctx context.Context, gc *gitea.Client, repositoryID string, issueID int, status bug.Status) error {
	var state string

	switch status {
	case bug.OpenStatus:
		state = "reopen"
	case bug.ClosedStatus:
		state = "close"
	default:
		panic("unknown bug state")
	}

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	_, _, err := gc.Issues.UpdateIssue(
		repositoryID, issueID,
		&gitea.UpdateIssueOptions{
			StateEvent: &state,
		},
		gitea.WithContext(ctx),
	)

	return err
}

func updateGiteaIssueBody(ctx context.Context, gc *gitea.Client, repositoryID string, issueID int, body string) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	gc.SetContext(ctx)
	defer cancel()
	_, _, err := gc.Issues.UpdateIssue(
		repositoryID, issueID,
		&gitea.UpdateIssueOptions{
			Description: &body,
		},
		gitea.WithContext(ctx),
	)

	return err
}

func updateGiteaIssueTitle(ctx context.Context, gc *gitea.Client, repositoryID string, issueID int, title string) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	gc.SetContext(ctx)
	defer cancel()
	_, _, err := gc.Issues.UpdateIssue(
		repositoryID, issueID,
		&gitea.UpdateIssueOptions{
			Title: &title,
		},
		gitea.WithContext(ctx),
	)

	return err
}

// update gitea. issue labels
func updateGiteaIssueLabels(ctx context.Context, gc *gitea.Client, repositoryID string, issueID int, labels []string) error {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	gc.SetContext(ctx)
	defer cancel()
	giteaLabels := gitea.Labels(labels)
	_, _, err := gc.Issues.UpdateIssue(
		repositoryID, issueID,
		&gitea.UpdateIssueOptions{
			Labels: &giteaLabels,
		},
		gitea.WithContext(ctx),
	)

	return err
}
