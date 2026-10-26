package bouncereader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultGraphURL is Microsoft Graph's v1.0 endpoint.
const DefaultGraphURL = "https://graph.microsoft.com/v1.0"

// pageSize is how many items one delta page asks for (Prefer:
// odata.maxpagesize); Graph may send fewer.
const pageSize = 50

// MaxMIME bounds one message the reader fetches: a report SkyMail asked for
// (RET=HDRS) is a few kilobytes, one that returns the whole mail is at most
// the size of a SkyMail mail (Graph's 4 MB) and its report. A larger one is
// not read and is counted as unreadable.
const MaxMIME = 10 << 20

// requestTimeout bounds one Graph request.
const requestTimeout = time.Minute

// tokenSource is the application's Graph token (msauth.TokenSource).
type tokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate(token string)
}

// graphClient reads the mailboxes through Graph: their Inbox's delta and an
// item's MIME.
type graphClient struct {
	base   string
	tokens tokenSource
	http   *http.Client
}

// message is an item of the Inbox's delta: only what the reader asks for
// ($select), never the subject, the sender or the body.
type message struct {
	ID                string    `json:"id"`
	InternetMessageID string    `json:"internetMessageId"`
	ReceivedDateTime  time.Time `json:"receivedDateTime"`
	Removed           *struct{} `json:"@removed"`
}

// deltaPage is one page of the delta: its items and either the next page's
// link or, on the last page, the delta link the next poll starts from.
type deltaPage struct {
	Value []message `json:"value"`
	Next  string    `json:"@odata.nextLink"`
	Delta string    `json:"@odata.deltaLink"`
}

// graphError is Graph's refusal: its HTTP status and error code, never its
// message.
type graphError struct {
	status int
	code   string
}

func (e *graphError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("graph: %d", e.status)
	}
	return fmt.Sprintf("graph: %d %s", e.status, e.code)
}

// errGone says the item is no longer in the mailbox (deleted between the
// delta and the fetch).
var errGone = errors.New("graph: the item is gone")

// errTooLarge says the item is larger than MaxMIME.
var errTooLarge = errors.New("graph: the item is larger than the reader reads")

// graphErrorCode keeps a Graph error code only if it looks like one.
var graphErrorCode = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]{0,63}$`)

// firstDelta is the link of a mailbox's first delta: its Inbox from the
// start, two properties of each item.
func (g *graphClient) firstDelta(mailbox string) string {
	return g.base + "/users/" + url.PathEscape(mailbox) + "/mailFolders/inbox/messages/delta?$select=internetMessageId,receivedDateTime"
}

// delta reads one page of a delta. link is firstDelta's or one Graph gave
// (a next or a delta link); a link to anywhere but Graph is refused, so the
// token never goes elsewhere.
func (g *graphClient) delta(ctx context.Context, link string) (deltaPage, error) {
	if !strings.HasPrefix(link, g.base+"/") {
		return deltaPage{}, errors.New("graph: a delta link that is not Graph's")
	}
	body, err := g.get(ctx, link, "application/json", 4<<20)
	if err != nil {
		return deltaPage{}, err
	}
	var page deltaPage
	if err := json.Unmarshal(body, &page); err != nil {
		return deltaPage{}, errors.New("graph: the delta page is not JSON")
	}
	for _, next := range []string{page.Next, page.Delta} {
		if next != "" && !strings.HasPrefix(next, g.base+"/") {
			return deltaPage{}, errors.New("graph: a delta link that is not Graph's")
		}
	}
	return page, nil
}

// mime reads an item's whole message (RFC 822), at most MaxMIME.
func (g *graphClient) mime(ctx context.Context, mailbox, id string) ([]byte, error) {
	link := g.base + "/users/" + url.PathEscape(mailbox) + "/messages/" + url.PathEscape(id) + "/$value"
	body, err := g.get(ctx, link, "", MaxMIME)
	var refused *graphError
	if errors.As(err, &refused) && refused.status == http.StatusNotFound {
		return nil, errGone
	}
	return body, err
}

// get makes one GET with the application's token and reads at most limit
// bytes of the answer; more is errTooLarge. A 401 drops the token, so the
// next request gets a new one.
func (g *graphClient) get(ctx context.Context, link, accept string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	token, err := g.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("graph token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, errors.New("graph: a link that is not a URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if accept != "" {
		req.Header.Set("Accept", accept)
		req.Header.Set("Prefer", fmt.Sprintf("odata.maxpagesize=%d", pageSize))
	}
	resp, err := g.http.Do(req)
	if err != nil {
		// Without the URL: only that Graph did not answer.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("graph: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if resp.StatusCode == http.StatusUnauthorized {
			g.tokens.Invalidate(token)
		}
		var parsed struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &parsed)
		refused := &graphError{status: resp.StatusCode}
		if graphErrorCode.MatchString(parsed.Error.Code) {
			refused.code = parsed.Error.Code
		}
		return nil, refused
	}
	var body bytes.Buffer
	n, err := io.Copy(&body, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("graph: read the answer: %w", err)
	}
	if n > limit {
		return nil, errTooLarge
	}
	return body.Bytes(), nil
}
