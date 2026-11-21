package logquery

import (
	"context"
	"sort"
	"strings"
	"time"
)

// List returns configured groups and files. It never reads file contents.
func (s *Service) List(ctx context.Context, req ListRequest) (res *ListResult, err error) {
	q, err := s.begin(ctx, "logs_list")
	if err != nil {
		return nil, err
	}
	res = &ListResult{Groups: []GroupInfo{}, Files: []FileInfo{}, Meta: newMeta()}
	defer func() {
		if res != nil {
			q.finish(&err, &res.Meta)
		} else {
			q.finish(&err, nil)
		}
	}()

	includeFiles := boolValue(req.IncludeFiles, true)
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > s.limits.MaxListPageSize {
		limit = s.limits.MaxListPageSize
		res.Meta.Warn("limit was capped by the server")
	}

	groups := s.reg.Groups()
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		res.Groups = append(res.Groups, GroupInfo{Name: name, FileCount: groups[name]})
	}

	if !includeFiles {
		res.Meta.Complete = true
		return res, nil
	}

	var modifiedSince time.Time
	if strings.TrimSpace(req.ModifiedWithin) != "" {
		d, derr := parseFlexibleDuration(req.ModifiedWithin)
		if derr != nil || d <= 0 {
			return nil, NewError(CodeInvalidArgument, "modified_within must be a positive duration such as 24h").
				WithHint(false, "Use values like 30m, 24h or 7d.")
		}
		modifiedSince = s.now().Add(-d)
	}

	group := strings.TrimSpace(req.Group)
	filterByGroup := group != "" && !strings.EqualFold(group, "all")
	if filterByGroup {
		if _, ok := groups[group]; !ok {
			return nil, NewError(CodeGroupNotFound, "the requested log group does not exist").
				WithHint(false, "Call logs_list without a group filter to discover groups.")
		}
	}

	var refs []FileRef
	for _, ref := range s.reg.List() {
		if filterByGroup && ref.Group != group {
			continue
		}
		if req.Pattern != "" {
			matched, merr := matchFilePattern(ref.Name, req.Pattern)
			if merr != nil {
				return nil, merr
			}
			if !matched {
				continue
			}
		}
		if !modifiedSince.IsZero() {
			if ref.ModTime <= 0 || time.Unix(ref.ModTime, 0).Before(modifiedSince) {
				continue
			}
		}
		refs = append(refs, ref)
	}
	if len(refs) > s.limits.MaxDiscoveryEntries {
		refs = refs[:s.limits.MaxDiscoveryEntries]
		res.Meta.Warn("file discovery was limited by max-discovery-entries")
	}

	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].ModTime != refs[j].ModTime {
			return refs[i].ModTime > refs[j].ModTime
		}
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].ID < refs[j].ID
	})

	digest := queryDigest("logs_list", group, req.Pattern, req.ModifiedWithin)
	if req.Cursor != "" {
		payload, cerr := s.cursors.Decode(req.Cursor)
		if cerr != nil {
			return nil, cerr
		}
		if payload.Tool != "logs_list" || payload.Query != digest {
			return nil, NewError(CodeInvalidCursor, "the cursor does not belong to this query")
		}
		refs = resumeAfter(refs, payload.AfterMtime, payload.AfterName, payload.AfterID)
	}

	res.Meta.ScannedFiles = len(refs)
	page := refs
	if len(page) > limit {
		page = page[:limit]
	}
	for _, ref := range page {
		res.Files = append(res.Files, FileInfo{
			FileID:     ref.ID,
			Group:      ref.Group,
			Name:       ref.Name,
			SizeBytes:  ref.Size,
			ModifiedAt: fileModifiedString(ref),
			Readable:   ref.Readable,
			Compressed: strings.HasSuffix(strings.ToLower(ref.Path), ".gz") ||
				strings.HasSuffix(strings.ToLower(ref.Path), ".gzip"),
		})
	}
	res.Meta.ReturnedCount = len(res.Files)

	if len(refs) > len(page) && len(page) > 0 {
		last := page[len(page)-1]
		token := s.cursors.Encode(cursorPayload{
			Tool:       "logs_list",
			Query:      digest,
			AfterMtime: last.ModTime,
			AfterName:  last.Name,
			AfterID:    last.ID,
		})
		res.HasMore = true
		res.NextCursor = &token
		res.Meta.HasMore = true
		res.Meta.NextCursor = &token
	}
	if res.HasMore {
		res.Meta.Complete = false
	}
	return res, nil
}

// resumeAfter returns the suffix of refs strictly after the (mtime, name, id)
// key in the listing's descending-mtime order.
func resumeAfter(refs []FileRef, mtime int64, name, id string) []FileRef {
	for i, ref := range refs {
		if ref.ModTime < mtime {
			return refs[i:]
		}
		if ref.ModTime == mtime {
			if ref.Name > name || (ref.Name == name && ref.ID > id) {
				return refs[i:]
			}
		}
	}
	return nil
}
