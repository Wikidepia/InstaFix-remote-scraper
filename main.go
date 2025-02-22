package main

import (
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unsafe"

	"github.com/CAFxX/httpcompression"
	"github.com/CAFxX/httpcompression/contrib/klauspost/zstd"
	"github.com/andybalholm/cascadia"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/kelindar/binary"
	"github.com/kelindar/binary/nocopy"
	"github.com/klauspost/compress/gzhttp"
	kpzstd "github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
	"go.mercari.io/go-dnscache"
	"golang.org/x/exp/rand"
	"golang.org/x/net/html"
)

type Media struct {
	TypeName nocopy.String
	URL      nocopy.String
}

type InstaData struct {
	PostID   nocopy.String
	Username nocopy.String
	Caption  nocopy.String
	Medias   []Media
}

// Copied from DefaultTransport
var (
	transport http.RoundTripper
	reqHeader http.Header
)

//go:embed dictionary.bin
var dict []byte

// b2s converts byte slice to a string without memory allocation.
// See https://groups.google.com/forum/#!msg/Golang-Nuts/ENgbUzYvCuU/90yGx7GUAgAJ .
func b2s(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

func init() {
	reqHeader = http.Header{}
	reqHeader.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:128.0) Gecko/20100101 Firefox/128.0")
	reqHeader.Set("Accept", "*/*")
	reqHeader.Set("Accept-Language", "en-US,en;q=0.5")
	reqHeader.Set("Content-Type", "application/x-www-form-urlencoded")
	reqHeader.Set("X-FB-Friendly-Name", "PolarisPostActionLoadPostQueryQuery")
	reqHeader.Set("Origin", "https://www.instagram.com")
	reqHeader.Set("DNT", "1")
	reqHeader.Set("Sec-GPC", "1")
	reqHeader.Set("Connection", "keep-alive")
	reqHeader.Set("Sec-Fetch-Dest", "empty")
	reqHeader.Set("Sec-Fetch-Mode", "cors")
	reqHeader.Set("Sec-Fetch-Site", "same-origin")
	reqHeader.Set("Pragma", "no-cache")
	reqHeader.Set("Cache-Control", "no-cache")
	reqHeader.Set("TE", "trailers")
}

func main() {
	resolver, err := dnscache.New(5*time.Minute, 5*time.Second)
	if err != nil {
		panic(err)
	}
	rand.Seed(uint64(time.Now().UTC().UnixNano()))

	transportCache := &http.Transport{
		// ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	cacheDialCtx := dnscache.DialFunc(resolver, nil)
	baseDialFunc := (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		DualStack: true,
	}).DialContext
	transportCache.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr == "www.instagram.com:443" {
			// IP is geo based, need to add some flag
			return baseDialFunc(ctx, network, "157.240.7.174:443")
		}
		return cacheDialCtx(ctx, network, addr)
	}
	transport = gzhttp.Transport(transportCache, gzhttp.TransportAlwaysDecompress(true))

	zdEnc, err := zstd.New(kpzstd.WithLowerEncoderMem(true), kpzstd.WithEncoderDict(dict), kpzstd.WithEncoderLevel(kpzstd.SpeedFastest))
	if err != nil {
		panic(err)
	}
	compressor, err := httpcompression.Adapter(
		httpcompression.Compressor("zstd.dict", 1, zdEnc),
	)
	if err != nil {
		panic(err)
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.ThrottleBacklog(20, 1000, 30*time.Second))
	r.Use(compressor)

	r.Mount("/debug", middleware.Profiler())
	r.Get("/scrape/{postID}", http.HandlerFunc(Scrape))

	err = http.ListenAndServe(":3001", r)
	if err != nil {
		panic(err)
	}
}

func Scrape(w http.ResponseWriter, r *http.Request) {
	postID := chi.URLParam(r, "postID")

	var err error
	if postID[0] == 'B' {
		postID, err = GetSharePostID(postID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	var idata *InstaData
	// 1. Scrape from graphql
	idata, err = ScrapeGQL(postID)
	if err != nil {
		slog.Error("failed to scrape from graphql", "postID", postID, "err", err)
		// 2. Scrape from page directly
		idata, err = ScrapePage(postID)
		if err != nil {
			slog.Error("failed to scrape from direct page", "postID", postID, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if len(idata.Username) == 0 {
		http.Error(w, "Post not found", http.StatusNotFound)
		return
	}

	err = binary.MarshalTo(idata, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func ScrapeGQL(postID string) (*InstaData, error) {
	params := url.Values{
		"variables":         {"{\"shortcode\":\"" + postID + "\",\"fetch_tagged_user_count\":null,\"hoisted_comment_id\":null,\"hoisted_reply_id\":null}"},
		"server_timestamps": {"true"},
		"doc_id":            {"8845758582119845"},
	}

	client := http.Client{
		Transport: transport,
	}
	req, err := http.NewRequest("POST", "https://www.instagram.com/graphql/query", strings.NewReader(params.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header = reqHeader

	buf := new(bytes.Buffer)
	var res *http.Response
	// TODO Sometimes api returns 5xx error, retrying doesn't help.
	for i := 0; i < 3; i++ {
		res, err = client.Do(req)
		if err != nil {
			continue
		}
		defer res.Body.Close()
		buf.Reset() // Reset buffer
		if _, err = buf.ReadFrom(res.Body); err != nil {
			continue
		}
		if bytes.Contains(buf.Bytes(), []byte("require_login")) {
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}

	if !bytes.Contains(buf.Bytes(), []byte("shortcode_media")) {
		return nil, errors.New("post not found")
	}

	data := gjson.Parse(b2s(buf.Bytes())).Get("data")

	var item gjson.Result
	if bytes.Contains(buf.Bytes(), []byte("xdt_shortcode_media")) {
		item = data.Get("xdt_shortcode_media")
	} else {
		item = data.Get("shortcode_media")
	}

	if item.Value() == nil {
		return nil, errors.New("shortcode_media is empty")
	}

	idata := &InstaData{
		PostID:   nocopy.String(postID),
		Username: nocopy.String(item.Get("owner.username").String()),
		Caption:  nocopy.String(item.Get("edge_media_to_caption.edges.0.node.text").String()),
	}

	// Get medias
	var media []gjson.Result
	if bytes.Contains(buf.Bytes(), []byte("edge_sidecar_to_children")) {
		media = item.Get("edge_sidecar_to_children.edges").Array()
	} else {
		media = []gjson.Result{item}
	}

	idata.Medias = make([]Media, 0, len(media))
	for _, m := range media {
		if m.Get("node").Exists() {
			m = m.Get("node")
		}
		mediaURL := m.Get("video_url")
		if !mediaURL.Exists() {
			mediaURL = m.Get("display_url")
		}
		idata.Medias = append(idata.Medias, Media{
			TypeName: nocopy.String(m.Get("__typename").String()),
			URL:      nocopy.String(mediaURL.String()),
		})
	}
	return idata, nil
}

func CSSQuery(n *html.Node, query string) *html.Node {
	sel, err := cascadia.Parse(query)
	if err != nil {
		return &html.Node{}
	}
	return cascadia.Query(n, sel)
}

func CSSAttrOr(n *html.Node, attrName, or string) string {
	for _, a := range n.Attr {
		if a.Key == attrName {
			return a.Val
		}
	}
	return or
}

func ScrapePage(postID string) (*InstaData, error) {
	client := http.Client{
		Transport: transport,
	}
	req, err := http.NewRequest("GET", "https://www.instagram.com/p/"+postID+"/", nil)
	if err != nil {
		return nil, err
	}

	req.Header = reqHeader

	buf := new(bytes.Buffer)
	var res *http.Response
	// TODO Sometimes api returns 5xx error, retrying doesn't help.
	for i := 0; i < 3; i++ {
		res, err = client.Do(req)
		if err != nil {
			continue
		}
		defer res.Body.Close()
		buf.Reset() // Reset buffer
		if _, err = buf.ReadFrom(res.Body); err != nil {
			continue
		}
		if bytes.Contains(buf.Bytes(), []byte("require_login")) {
			continue
		}
		break
	}
	if err != nil {
		return nil, err
	}

	doc, err := html.Parse(buf)
	if err != nil {
		return nil, err
	}

	// Get username
	twitterTitle := CSSQuery(doc, `meta[name="twitter:title"]`)
	if twitterTitle == nil {
		return nil, errors.New("twitter:title not found")
	}

	twTitleContent := CSSAttrOr(twitterTitle, "content", "")
	if twTitleContent == "" {
		return nil, errors.New("twitter:title content is empty")
	}

	// real name (@username) • Instagram reel
	var username string
	usernameLeft := strings.Split(twTitleContent, "(")
	if len(usernameLeft) == 2 {
		usernameRight := strings.Split(usernameLeft[1], ")")
		username = strings.Trim(usernameRight[0], "@")
	} else {
		return nil, errors.New("username not found")
	}

	// Get caption
	ogDescription := CSSQuery(doc, `meta[name="description"]`)
	if ogDescription == nil {
		return nil, errors.New("og:description not found")
	}

	descriptionContent := CSSAttrOr(ogDescription, "content", "no caption")

	// xxx likes ... : "<real caption>"
	var caption string
	captionTrim := strings.Split(descriptionContent, ":")
	if len(captionTrim) == 2 {
		caption = strings.Trim(captionTrim[1], ": ")
	}

	idata := &InstaData{
		PostID:   nocopy.String(postID),
		Username: nocopy.String(username),
		Caption:  nocopy.String(caption),
	}
	idata.Medias = append(idata.Medias, Media{
		TypeName: nocopy.String("GraphImage"),
		URL:      nocopy.String(CSSAttrOr(CSSQuery(doc, `meta[property="og:image"]`), "content", "no image")),
	})
	return idata, nil
}

func GetSharePostID(postID string) (string, error) {
	req, err := http.NewRequest("HEAD", "https://www.instagram.com/share/reel/"+postID+"/", nil)
	if err != nil {
		return postID, err
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return postID, err
	}
	defer resp.Body.Close()
	redirURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return postID, err
	}
	if path.Base(redirURL.Path) == "login" {
		return postID, errors.New("not logged in")
	}
	return path.Base(redirURL.Path), nil
}
