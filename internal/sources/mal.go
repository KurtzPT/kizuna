package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"kizuna/internal/fetch"
)

const malAPI = "https://api.myanimelist.net/v2"

// MAL reads public MyAnimeList data through the official API v2.
// Only a client ID is needed (myanimelist.net/apiconfig), no user login.
// Used only as a fallback: summary and poster when AniDB has none.
type MAL struct {
	f        *fetch.Client
	clientID string
	keys     *keyCheck
}

func NewMAL(f *fetch.Client, clientID string) *MAL {
	f.Throttle("api.myanimelist.net", 500*time.Millisecond)
	keys := newKeyCheck("MyAnimeList", "MAL_CLIENT_ID")
	keys.rejects = malBadClient
	return &MAL{f: f, clientID: clientID, keys: keys}
}

// malBadClient: MyAnimeList answers a wrong client id with HTTP 400
// {"message":"Invalid client id","error":"bad_request"}, not 401/403. Other
// 400s (a bad request of ours) say nothing about the client id.
func malBadClient(se *fetch.StatusError) bool {
	if se.Code != http.StatusBadRequest {
		return false
	}
	var r struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	return json.Unmarshal(se.Body, &r) == nil && r.Error == "bad_request" && strings.EqualFold(r.Message, "invalid client id")
}

func (m *MAL) Enabled() bool { return m != nil && m.clientID != "" } // nil = not set up

// MALAnime is the part of a MyAnimeList entry we use.
type MALAnime struct {
	Synopsis string
	Poster   string
	Details  Details // genres, studio, content rating, start date (no people in the API)
}

// malRatings maps MAL's rating codes to the usual (US) labels.
var malRatings = map[string]string{"g": "G", "pg": "PG", "pg_13": "PG-13", "r": "R", "r+": "R+", "rx": "X"}

func (m *MAL) Anime(ctx context.Context, id int) (MALAnime, error) {
	if !m.Enabled() {
		return MALAnime{}, fmt.Errorf("mal: no client id")
	}
	var r struct {
		Synopsis string `json:"synopsis"`
		Picture  struct {
			Large  string `json:"large"`
			Medium string `json:"medium"`
		} `json:"main_picture"`
		Genres []struct {
			Name string `json:"name"`
		} `json:"genres"`
		Studios []struct {
			Name string `json:"name"`
		} `json:"studios"`
		Start  string `json:"start_date"`
		Rating string `json:"rating"`
	}
	err := m.keys.getJSON(ctx, m.f, fetch.Request{
		URL:     fmt.Sprintf("%s/anime/%d?fields=synopsis,main_picture,genres,studios,start_date,rating", malAPI, id),
		Key:     fmt.Sprintf("mal/v2-%d-full.json", id),
		TTL:     30 * 24 * time.Hour,
		Headers: map[string]string{"X-MAL-CLIENT-ID": m.clientID},
	}, &r)
	if err != nil {
		return MALAnime{}, err
	}
	syn := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(r.Synopsis), "[Written by MAL Rewrite]"))
	poster := r.Picture.Large
	if poster == "" {
		poster = r.Picture.Medium
	}
	d := Details{Date: r.Start}
	d.addRating("US", malRatings[r.Rating]) // MAL uses the US (MPAA-style) system
	for _, g := range r.Genres {
		d.Genres = append(d.Genres, g.Name)
	}
	if len(r.Studios) > 0 {
		d.Studio = r.Studios[0].Name
	}
	return MALAnime{Synopsis: syn, Poster: poster, Details: d}, nil
}
