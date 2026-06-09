package filter

type FilterAction struct {
	Type     string
	Strategy string
	Reason   string
	Original string
	Language string
	ToolName string
	Path     string
	Input    string
	Filtered bool
}

type FilterResult struct {
	Content  string
	Actions  []FilterAction
	Filtered bool
}
