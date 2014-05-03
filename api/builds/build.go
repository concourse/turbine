package builds

type Build struct {
	Guid string `json:"guid"`

	Image  string      `json:"image"`
	Env    [][2]string `json:"env"`
	Script string      `json:"script"`

	LogsURL  string `json:"logs_url"`
	Callback string `json:"callback"`

	Source BuildSource `json:"source"`

	Status string `json:"status"`
}

type BuildSource struct {
	Type   string `json:"type"`
	URI    string `json:"uri"`
	Branch string `json:"branch"`
	Ref    string `json:"ref"`
	Path   string `json:"path"`
}
