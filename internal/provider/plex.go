package provider

// Plex custom metadata provider JSON shapes.
// Spec: https://developer.plex.tv/pms/#section/API-Info/Metadata-Providers

type Container struct {
	MediaContainer MediaContainer `json:"MediaContainer"`
}

type MediaContainer struct {
	Offset     int     `json:"offset"`
	TotalSize  int     `json:"totalSize"`
	Size       int     `json:"size"`
	Identifier string  `json:"identifier"`
	Metadata   []Item  `json:"Metadata,omitempty"`
	Image      []Image `json:"Image,omitempty"`
}

// Item is a show, season or episode.
type Item struct {
	RatingKey string `json:"ratingKey"`
	Key       string `json:"key"`
	GUID      string `json:"guid"`
	Type      string `json:"type"` // show | season | episode
	Title     string `json:"title"`
	Score     int    `json:"score,omitempty"` // match confidence (Fix Match list)

	OriginalTitle         string `json:"originalTitle,omitempty"`
	Summary               string `json:"summary,omitempty"`
	Year                  int    `json:"year,omitempty"`
	OriginallyAvailableAt string `json:"originallyAvailableAt,omitempty"`
	Studio                string `json:"studio,omitempty"`
	ContentRating         string `json:"contentRating,omitempty"`
	Duration              int    `json:"duration,omitempty"` // milliseconds
	Thumb                 string `json:"thumb,omitempty"`
	Art                   string `json:"art,omitempty"`

	// Index is a pointer so season 0 (Specials) is still sent.
	Index       *int `json:"index,omitempty"`
	ParentIndex *int `json:"parentIndex,omitempty"`

	ParentRatingKey      string `json:"parentRatingKey,omitempty"`
	ParentKey            string `json:"parentKey,omitempty"`
	ParentGUID           string `json:"parentGuid,omitempty"`
	ParentType           string `json:"parentType,omitempty"`
	ParentTitle          string `json:"parentTitle,omitempty"`
	ParentThumb          string `json:"parentThumb,omitempty"`
	GrandparentRatingKey string `json:"grandparentRatingKey,omitempty"`
	GrandparentKey       string `json:"grandparentKey,omitempty"`
	GrandparentGUID      string `json:"grandparentGuid,omitempty"`
	GrandparentType      string `json:"grandparentType,omitempty"`
	GrandparentTitle     string `json:"grandparentTitle,omitempty"`
	GrandparentThumb     string `json:"grandparentThumb,omitempty"`

	Genre    []Tag     `json:"Genre,omitempty"`
	Director []Tag     `json:"Director,omitempty"`
	Writer   []Tag     `json:"Writer,omitempty"`
	Role     []Role    `json:"Role,omitempty"`
	Guid     []GuidTag `json:"Guid,omitempty"`
	Image    []Image   `json:"Image,omitempty"`
	Children *Children `json:"Children,omitempty"`
}

type Tag struct {
	Tag string `json:"tag"`
}

type Role struct {
	Tag   string `json:"tag"`            // actor (seiyuu)
	Role  string `json:"role,omitempty"` // character
	Thumb string `json:"thumb,omitempty"`
	Order int    `json:"order"`
}

type GuidTag struct {
	ID string `json:"id"` // tvdb://123, tmdb://456, imdb://tt789
}

type Image struct {
	Type string `json:"type"` // coverPoster | background | clearLogo | snapshot
	URL  string `json:"url"`
	Alt  string `json:"alt,omitempty"`
}

type Children struct {
	Size     int    `json:"size"`
	Metadata []Item `json:"Metadata"`
}

// MediaProvider is the answer to GET /<mode>.
type MediaProvider struct {
	MediaProvider struct {
		Identifier string    `json:"identifier"`
		Title      string    `json:"title"`
		Version    string    `json:"version"`
		Types      []Type    `json:"Types"`
		Feature    []Feature `json:"Feature"`
	} `json:"MediaProvider"`
}

type Type struct {
	Type   int      `json:"type"` // 2 show, 3 season, 4 episode
	Scheme []Scheme `json:"Scheme"`
}

type Scheme struct {
	Scheme string `json:"scheme"`
}

type Feature struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

func intp(n int) *int { return &n }
