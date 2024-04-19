package link2json

type MetaDataResponseItem struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Images      []WebImage `json:"images"`
	Sitename    string     `json:"sitename"`
	Favicon     string     `json:"favicon"`
	Duration    int        `json:"duration"`
	Domain      string     `json:"domain"`
	URL         string     `json:"url"`
}

type WebImage struct {
	URL    string `json:"url"`
	Alt    string `json:"alt,omitempty"`
	Type   string `json:"type,omitempty"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}
