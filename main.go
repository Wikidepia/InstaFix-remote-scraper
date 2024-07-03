package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/kelindar/binary"
	"github.com/kelindar/binary/nocopy"
	"github.com/klauspost/compress/gzhttp"
	"github.com/rs/dnscache"
	"github.com/tidwall/gjson"
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

var resolver = &dnscache.Resolver{
	Resolver: &net.Resolver{
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{}
			return d.DialContext(ctx, "udp", "8.8.8.8:53")
		},
	},
}
var transport = &http.Transport{
	DialContext: func(ctx context.Context, network string, addr string) (conn net.Conn, err error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := resolver.LookupHost(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			var dialer net.Dialer
			conn, err = dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
			if err == nil {
				break
			}
		}
		return
	},
}
var header = http.Header{
	"accept":                      {"*/*"},
	"accept-language":             {"en-US,en;q=0.9"},
	"content-type":                {"application/x-www-form-urlencoded"},
	"origin":                      {"https://www.instagram.com"},
	"priority":                    {"u=1, i"},
	"sec-ch-prefers-color-scheme": {"dark"},
	"sec-ch-ua":                   {`"Google Chrome";v="125", "Chromium";v="125", "Not.A/Brand";v="24"`},
	"sec-ch-ua-full-version-list": {`"Google Chrome";v="125.0.6422.142", "Chromium";v="125.0.6422.142", "Not.A/Brand";v="24.0.0.0"`},
	"sec-ch-ua-mobile":            {"?0"},
	"sec-ch-ua-model":             {`""`},
	"sec-ch-ua-platform":          {`"macOS"`},
	"sec-ch-ua-platform-version":  {`"12.7.4"`},
	"sec-fetch-dest":              {"empty"},
	"sec-fetch-mode":              {"cors"},
	"sec-fetch-site":              {"same-origin"},
	"user-agent":                  {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"},
	"x-asbd-id":                   {"129477"},
	"x-bloks-version-id":          {"e2004666934296f275a5c6b2c9477b63c80977c7cc0fd4b9867cb37e36092b68"},
	"x-fb-friendly-name":          {"PolarisPostActionLoadPostQueryQuery"},
	"x-ig-app-id":                 {"936619743392459"},
}

func main() {
	// Clear dnscache every 5 minutes
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for range t.C {
			resolver.Refresh(true)
		}
	}()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	handler, err := gzhttp.NewWrapper(gzhttp.MinSize(0))
	if err != nil {
		println(err)
	}
	r.Get("/scrape/{postID}", handler(http.HandlerFunc(Scrape)))

	err = http.ListenAndServe(":3001", r)
	if err != nil {
		println(err)
	}
}

func Scrape(w http.ResponseWriter, r *http.Request) {
	postID := chi.URLParam(r, "postID")

	var i InstaData
	var data gjson.Result

	// 1. Use Embed
	// 2. Scrape from graphql
	response, err := ParseGQL(postID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data = gjson.ParseBytes(response).Get("data")

	item := data.Get("shortcode_media")
	if !item.Exists() {
		item = data.Get("xdt_shortcode_media")
		if !item.Exists() {
			http.Error(w, "Post not found", http.StatusNotFound)
			return
		}
	}

	media := []gjson.Result{item}
	if item.Get("edge_sidecar_to_children").Exists() {
		media = item.Get("edge_sidecar_to_children.edges").Array()
	}

	i.PostID = nocopy.String(postID)

	// Get username
	i.Username = nocopy.String(item.Get("owner.username").String())

	// Get caption
	i.Caption = nocopy.String(item.Get("edge_media_to_caption.edges.0.node.text").String())

	// Get medias
	i.Medias = make([]Media, 0, len(media))
	for _, m := range media {
		if m.Get("node").Exists() {
			m = m.Get("node")
		}
		mediaURL := m.Get("video_url")
		if !mediaURL.Exists() {
			mediaURL = m.Get("display_url")
		}
		i.Medias = append(i.Medias, Media{
			TypeName: nocopy.String(m.Get("__typename").String()),
			URL:      nocopy.String(mediaURL.String()),
		})
	}

	err = binary.MarshalTo(i, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func ParseGQL(postID string) ([]byte, error) {
	gqlParams := url.Values{
		"av":                       {"0"},
		"__d":                      {"www"},
		"__user":                   {"0"},
		"__a":                      {"1"},
		"__req":                    {"k"},
		"__hs":                     {"19888.HYP:instagram_web_pkg.2.1..0.0"},
		"dpr":                      {"2"},
		"__ccg":                    {"UNKNOWN"},
		"__rev":                    {"1014227545"},
		"__s":                      {"trbjos:n8dn55:yev1rm"},
		"__hsi":                    {"7380500578385702299"},
		"__dyn":                    {"7xeUjG1mxu1syUbFp40NonwgU7SbzEdF8aUco2qwJw5ux609vCwjE1xoswaq0yE6ucw5Mx62G5UswoEcE7O2l0Fwqo31w9a9wtUd8-U2zxe2GewGw9a362W2K0zK5o4q3y1Sx-0iS2Sq2-azo7u3C2u2J0bS1LwTwKG1pg2fwxyo6O1FwlEcUed6goK2O4UrAwCAxW6Uf9EObzVU8U"},
		"__csr":                    {"n2Yfg_5hcQAG5mPtfEzil8Wn-DpKGBXhdczlAhrK8uHBAGuKCJeCieLDyExenh68aQAKta8p8ShogKkF5yaUBqCpF9XHmmhoBXyBKbQp0HCwDjqoOepV8Tzk8xeXqAGFTVoCciGaCgvGUtVU-u5Vp801nrEkO0rC58xw41g0VW07ISyie2W1v7F0CwYwwwvEkw8K5cM0VC1dwdi0hCbc094w6MU1xE02lzw"},
		"__comet_req":              {"7"},
		"lsd":                      {"AVoPBTXMX0Y"},
		"jazoest":                  {"2882"},
		"__spin_r":                 {"1014227545"},
		"__spin_b":                 {"trunk"},
		"__spin_t":                 {"1718406700"},
		"fb_api_caller_class":      {"RelayModern"},
		"fb_api_req_friendly_name": {"PolarisPostActionLoadPostQueryQuery"},
		"variables":                {`{"shortcode":"` + postID + `"}`},
		"server_timestamps":        {"true"},
		"doc_id":                   {"25531498899829322"},
	}

	client := http.Client{
		Transport: transport,
	}
	req, err := http.NewRequest("POST", "https://www.instagram.com/graphql/query", strings.NewReader(gqlParams.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header = header

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	gqlResponse, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return gqlResponse, nil
}
