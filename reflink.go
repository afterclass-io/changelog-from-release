package main

// Note: https://docs.github.com/en/get-started/writing-on-github/working-with-advanced-formatting/autolinked-references-and-urls

import (
	"bytes"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

type refLink struct {
	start int
	end   int
	text  string
}

type byStart []refLink

func (l byStart) Len() int           { return len(l) }
func (l byStart) Swap(i, j int)      { l[i], l[j] = l[j], l[i] }
func (l byStart) Less(i, j int) bool { return l[i].start < l[j].start }

// Note: '_' is actually not boundary. But it's hard to check if the '_' is a part of italic/bold
// syntax.
// For example, _#123_ should be linked because '_'s are part of italic syntax. But _#123 and #123_
// should not be linked because '_'s are NOT part of italic syntax.
// Checking if the parent node is Italic/Bold or not does not help to solve this issue. For example,
// _foo_#1 should be linked. However #1 itself is not an italic text though the neighbor node is
// Italic.
// Fortunately this is very edge case. To keep our implementation simple, we compromise to treat '_'
// as a boundary. For example, _#1 and #1_ are linked incorrectly, but I believe they are OK for our
// use cases.
func isBoundary(b byte) bool {
	if '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' {
		return false
	}
	return true
}

func isUserNameChar(b byte) bool {
	return '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || b == '-'
}

type extRef struct {
	prefix string
	pat    *regexp.Regexp
	url    string
}

// Reflinker detects all references in markdown text and replaces them with links.
type Reflinker struct {
	repo  string
	home  string
	src   []byte
	ext   []extRef
	links []refLink
}

// NewReflinker creates Reflinker instance. repoURL is a repository URL of the service like
// https://github.com/user/repo.
func NewReflinker(repoURL string) *Reflinker {
	u, err := url.Parse(repoURL)
	if err != nil {
		panic(err)
	}
	u.Path = ""

	l := &Reflinker{
		repo: repoURL,
		home: u.String(),
	}
	l.AddExtRef("GH-", repoURL+"/issues/<num>", false)
	return l
}

func (l *Reflinker) reset(src []byte) {
	l.src = src
	l.links = nil
}

func (l *Reflinker) isBoundaryAt(idx int) bool {
	if idx < 0 || len(l.src) <= idx {
		return true
	}
	return isBoundary(l.src[idx])
}

func (l *Reflinker) lastIndexIssueRef(begin, end int) int {
	if !l.isBoundaryAt(begin - 1) {
		return -1 // Issue ref must follow a boundary (e.g. 'foo#bar')
	}

	for i := 1; begin+i < end; i++ {
		b := l.src[begin+i]
		if '0' <= b && b <= '9' {
			continue
		}
		if i == 1 || !isBoundary(b) {
			return -1
		}
		return begin + i
	}

	if !l.isBoundaryAt(end) {
		return -1
	}

	return end // The text ends with issue number
}

func (l *Reflinker) linkIssueRef(begin, end int) int {
	e := l.lastIndexIssueRef(begin, end)
	if e < 0 {
		return begin + 1
	}

	r := l.src[begin:e]
	l.links = append(l.links, refLink{
		start: begin,
		end:   e,
		// Note: The link may be for PR, but GitHub can redirect this issue link to the PR
		text: fmt.Sprintf("[%s](%s/issues/%s)", r, l.repo, r[1:]),
	})

	return e
}

func (l *Reflinker) lastIndexUserRef(begin, end int) int {
	if !l.isBoundaryAt(begin - 1) {
		return -1 // e.g. foo@bar, _@foo (-@foo is ok)
	}

	// Note: Username may only contain alphanumeric characters or single hyphens, and cannot begin
	// or end with a hyphen: @foo-, @-foo
	// Note: '/' just after user name like @foo/ is not allowed

	if b := l.src[begin+1]; !isUserNameChar(b) || b == '-' {
		return -1
	}

	for i := 2; begin+i < end; i++ {
		b := l.src[begin+i]
		if isUserNameChar(b) {
			continue
		}
		if !isBoundary(b) || b == '/' || l.src[begin+i-1] == '-' {
			return -1
		}
		return begin + i
	}

	if l.src[end-1] == '-' {
		return -1
	}
	if end < len(l.src) {
		if b := l.src[end]; !isBoundary(b) || b == '/' {
			return -1
		}
	}

	return end
}

func (l *Reflinker) linkUserRef(begin, end int) int {
	e := l.lastIndexUserRef(begin, end)
	if e < 0 {
		return begin + 1
	}

	u := l.src[begin:e]
	l.links = append(l.links, refLink{
		start: begin,
		end:   e,
		text:  fmt.Sprintf("[%s](%s/%s)", u, l.home, u[1:]),
	})

	return e
}

const hashLen int = 40

func (l *Reflinker) linkCommitSHA(begin, end int) int {
	for i := 1; i < hashLen; i++ { // Since l.src[begin] was already checked, i starts from 1
		if begin+i >= end {
			return begin + i
		}
		b := l.src[begin+i]
		if '0' <= b && b <= '9' || 'a' <= b && b <= 'f' {
			continue
		}
		return begin + i
	}

	if l.isBoundaryAt(begin-1) && l.isBoundaryAt(begin+hashLen) {
		h := l.src[begin : begin+hashLen]
		l.links = append(l.links, refLink{
			start: begin,
			end:   begin + hashLen,
			text:  fmt.Sprintf("[`%s`](%s/commit/%s)", h[:10], l.repo, h),
		})
	}

	return begin + hashLen
}

func (l *Reflinker) linkGitHubRefs(t *ast.Text) {
	o := t.Segment.Start // start offset

	for o < t.Segment.Stop-1 { // `-1` means the last character is not checked
		s := l.src[o:t.Segment.Stop]
		i := bytes.IndexAny(s, "#@1234567890abcdef")
		if i < 0 || len(s)-1 <= i {
			return
		}

		switch s[i] {
		case '#':
			o = l.linkIssueRef(o+i, t.Segment.Stop)
		case '@':
			o = l.linkUserRef(o+i, t.Segment.Stop)
		default:
			// hex character [0-9a-f]
			o = l.linkCommitSHA(o+i, t.Segment.Stop)
		}
	}
}

// Parameters are corresponding to the API:
// https://docs.github.com/en/rest/repos/autolinks?apiVersion=2022-11-28
func (l *Reflinker) AddExtRef(prefix, url string, alphanumeric bool) {
	var r *regexp.Regexp
	if alphanumeric {
		r = regexp.MustCompile(`\b` + prefix + `[a-zA-Z0-9_]+`)
	} else {
		r = regexp.MustCompile(`\b` + prefix + `\d+\b`)
	}
	l.ext = append(l.ext, extRef{prefix, r, url})
}

func (l *Reflinker) linkExtRef(start, end int) int {
	src := l.src[start:end]
	for _, ext := range l.ext {
		if r := ext.pat.FindIndex(src); r != nil {
			s, e := r[0], r[1]
			ref := src[s:e]
			num := ref[len(ext.prefix):]
			url := strings.ReplaceAll(ext.url, "<num>", string(num))
			l.links = append(l.links, refLink{
				start: start + s,
				end:   start + e,
				text:  fmt.Sprintf("[%s](%s)", ref, url),
			})
			return start + e
		}
	}
	return end // Not found
}

func (l *Reflinker) linkExtRefs(t *ast.Text) {
	o := t.Segment.Start
	for o < t.Segment.Stop-1 {
		o = l.linkExtRef(o, t.Segment.Stop)
	}
}

// Commit URL with fragment should not be converted to a reference link.
// e.g. https://github.com/rhysd/changelog-from-release/commit/096c8152092281371e88265dd43b1b7d23a88453#diff-ced928ba39db1f56ef7862baebfe0314ed06f433a71defdc60a2b12e67011453L226
var reGitHubCommitPath = regexp.MustCompile(`^/([^/]+/[^/]+)/commit/([[:xdigit:]]{7,})$`)

func (l *Reflinker) linkCommitURL(m [][]byte, url []byte, start, end int) {
	slug, hash := m[1], m[2]
	if len(hash) > 10 {
		hash = hash[:10]
	}

	var replaced string
	if bytes.HasPrefix(url, []byte(l.repo)) {
		replaced = fmt.Sprintf("[`%s`](%s)", hash, url)
	} else {
		replaced = fmt.Sprintf("[%s@`%s`](%s)", slug, hash, url)
	}

	l.links = append(l.links, refLink{
		start: start,
		end:   end,
		text:  replaced,
	})
}

// Consider URL with fragment which links to issue comments.
// e.g.
// - https://github.com/rhysd/changelog-from-release/issues/11#issue-1327166917
// - https://github.com/rhysd/changelog-from-release/issues/11#issuecomment-1346614286
// - https://github.com/rhysd/changelog-from-release/pull/15#pullrequestreview-1212591132
// - https://github.com/rhysd/changelog-from-release/pull/15#discussion_r1045110870
var reGitHubIssuePath = regexp.MustCompile(`^/([^/]+/[^/]+)/(?:pull|issues)/(\d+)(#.+)?$`)

func (l *Reflinker) linkIssueURL(m [][]byte, url []byte, start, end int) {
	slug, num := m[1], m[2]

	// When hash like #issue-12345 follows, it links to a comment in the issue thread
	var note string
	if len(m[3]) > 0 {
		if bytes.HasPrefix(m[3], []byte("#pullrequestreview-")) {
			note = " (review)"
		} else {
			note = " (comment)"
		}
	}

	var replaced string
	if bytes.HasPrefix(url, []byte(l.repo)) {
		replaced = fmt.Sprintf("[#%s%s](%s)", num, note, url)
	} else {
		replaced = fmt.Sprintf("[%s#%s%s](%s)", slug, num, note, url)
	}

	l.links = append(l.links, refLink{
		start: start,
		end:   end,
		text:  replaced,
	})
}

func (l *Reflinker) linkURL(n *ast.AutoLink) {
	start := 0
	if p := n.PreviousSibling(); p != nil {
		t := p.(*ast.Text)
		if t == nil {
			return
		}
		start = t.Segment.Stop
	}

	home := []byte(l.home)
	url := n.URL(l.src)
	if !bytes.HasPrefix(url, home) {
		return
	}

	// Search the offset of the start of the URL. When the text is a child of some other node, URL
	// may not appear just after the previous node. The example is **https://...** where URL appers
	// after the first **.
	offset := bytes.Index(l.src[start:], url)
	if offset < 0 {
		return
	}
	start += offset

	end := start + len(url)
	if start >= len(l.src) || end > len(l.src) {
		return
	}

	// Note: `end` is the index of the character just after the URL
	if start > 0 && l.src[start-1] == '<' && end < len(l.src) && l.src[end] == '>' {
		return
	}

	path := url[len(home):]

	if m := reGitHubCommitPath.FindSubmatch(path); m != nil {
		l.linkCommitURL(m, url, start, end)
	} else if m := reGitHubIssuePath.FindSubmatch(path); m != nil {
		l.linkIssueURL(m, url, start, end)
	}
}

func (l *Reflinker) buildLinkedText() string {
	sort.Sort(byStart(l.links))

	var b strings.Builder
	i := 0
	for _, r := range l.links {
		b.Write(l.src[i:r.start])
		b.WriteString(r.text)
		i = r.end
	}
	b.Write(l.src[i:])
	return b.String()
}

func (l *Reflinker) isLinkDetected() bool {
	return len(l.links) > 0
}

// Link replaces all references in the given markdown text with actual links.
func (l *Reflinker) Link(input string) string {
	src := []byte(input)
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	t := md.Parser().Parse(text.NewReader(src))
	l.reset(src)

	ast.Walk(t, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		switch n := n.(type) {
		case *ast.CodeSpan, *ast.Link:
			return ast.WalkSkipChildren, nil
		case *ast.AutoLink:
			l.linkURL(n)
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			l.linkGitHubRefs(n)
			l.linkExtRefs(n)
			return ast.WalkContinue, nil
		default:
			return ast.WalkContinue, nil
		}
	})

	if !l.isLinkDetected() {
		return input
	}

	return l.buildLinkedText()
}
