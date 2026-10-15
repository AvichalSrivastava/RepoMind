package confluence

// pageDTO mirrors GET /rest/api/content/{id}?expand=body.storage,version
// for the fields we care about. Confluence returns many more fields;
// they are intentionally ignored so a backward-compatible field
// addition upstream does not break us.
type pageDTO struct {
	ID      string     `json:"id"`
	Type    string     `json:"type"`
	Status  string     `json:"status"`
	Title   string     `json:"title"`
	Version versionDTO `json:"version"`
	Body    bodyDTO    `json:"body"`
	Links   linksDTO   `json:"_links"`
}

type versionDTO struct {
	Number int `json:"number"`
}

type bodyDTO struct {
	Storage storageDTO `json:"storage"`
}

type storageDTO struct {
	Value          string `json:"value"`
	Representation string `json:"representation"`
}

type linksDTO struct {
	Base   string `json:"base"`
	WebUI  string `json:"webui"`
	Self   string `json:"self"`
	TinyUI string `json:"tinyui"`
}

// updatePageRequestDTO is the body for PUT /rest/api/content/{id}.
// The Confluence API requires title, version.number (new value), and
// the body in either storage or wiki format.
type updatePageRequestDTO struct {
	ID      string               `json:"id"`
	Type    string               `json:"type"`
	Title   string               `json:"title"`
	Version updatePageVersionDTO `json:"version"`
	Body    updatePageBodyDTO    `json:"body"`
}

type updatePageVersionDTO struct {
	Number int `json:"number"`
}

type updatePageBodyDTO struct {
	Storage storageDTO `json:"storage"`
}

// createPageRequestDTO is the body for POST /rest/api/content.
// Confluence Cloud creates a page as a child of an ancestor (page
// OR folder — v1 REST treats folders as page-like when referenced
// via ancestors[].id).
type createPageRequestDTO struct {
	Type      string              `json:"type"`      // "page"
	Title     string              `json:"title"`     // display title
	Space     createSpaceDTO      `json:"space"`     // {"key": "SHL360"}
	Ancestors []createAncestorDTO `json:"ancestors"` // parent page/folder
	Body      updatePageBodyDTO   `json:"body"`      // initial storage body
}

type createSpaceDTO struct {
	Key string `json:"key"`
}

type createAncestorDTO struct {
	ID string `json:"id"`
}
