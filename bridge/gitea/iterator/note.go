package iterator

import (
	"context"

	"code.gitea.io/sdk/gitea"
)

type noteIterator struct {
	issue    int
	page     int
	lastPage bool
	index    int
	cache    []*gitea.Note
}

func newNoteIterator() *noteIterator {
	in := &noteIterator{}
	in.Reset(-1)
	return in
}

func (in *noteIterator) Next(ctx context.Context, conf config) (bool, error) {
	// first query
	if in.cache == nil {
		return in.getNext(ctx, conf)
	}

	// move cursor index
	if in.index < len(in.cache)-1 {
		in.index++
		return true, nil
	}

	return in.getNext(ctx, conf)
}

func (in *noteIterator) Value() *gitea.Note {
	return in.cache[in.index]
}

func (in *noteIterator) getNext(ctx context.Context, conf config) (bool, error) {
	if in.lastPage {
		return false, nil
	}

	ctx, cancel := context.WithTimeout(ctx, conf.timeout)
	conf.gc.SetContext(ctx)
	defer cancel()

	notes, resp, err := conf.gc.Notes.ListIssueNotes(
		conf.project,
		in.issue,
		&gitea.ListIssueNotesOptions{
			ListOptions: gitea.ListOptions{
				Page:     in.page,
				PageSize: conf.capacity,
			},
			Sort:    gitea.String("asc"),
			OrderBy: gitea.String("created_at"),
		},
	)

	if err != nil {
		in.Reset(-1)
		return false, err
	}

	if resp.TotalPages == in.page {
		in.lastPage = true
	}

	if len(notes) == 0 {
		return false, nil
	}

	in.cache = notes
	in.index = 0
	in.page++

	return true, nil
}

func (in *noteIterator) Reset(issue int) {
	in.issue = issue
	in.index = -1
	in.page = 1
	in.lastPage = false
	in.cache = nil
}
