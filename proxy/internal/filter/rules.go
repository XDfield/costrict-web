package filter

type FilterRules struct {
	DefaultStrategy       string                 `yaml:"default_strategy"`
	PartStrategies        PartStrategiesConfig   `yaml:"part_strategies"`
	AllowedLanguages      []string               `yaml:"allowed_languages"`
	AllowedTools          []string               `yaml:"allowed_tools"`
	ShellCharThreshold    int                    `yaml:"shell_char_threshold"`
	ReasoningThreshold    int                    `yaml:"reasoning_threshold"`
	ContentLengthThreshold int                   `yaml:"content_length_threshold"`
	PreserveLanguageHint  bool                   `yaml:"preserve_language_hint"`
	PreserveFilePaths     bool                   `yaml:"preserve_file_paths"`
	RedactPlaceholder     string                 `yaml:"redact_placeholder"`
	AuditPassthrough      bool                   `yaml:"audit_passthrough"`
}

type PartStrategiesConfig struct {
	Text     TextStrategyConfig     `yaml:"text"`
	Tool     ToolStrategyConfig     `yaml:"tool"`
	Runtime  RuntimeStrategyConfig  `yaml:"runtime"`
}

type TextStrategyConfig struct {
	DefaultStrategy string `yaml:"default_strategy"`
}

type ToolStrategyConfig struct {
	DefaultStrategy string            `yaml:"default_strategy"`
	ToolOverrides   map[string]string `yaml:"tool_overrides"`
}

type RuntimeStrategyConfig struct {
	FileContent  StrategyConfig `yaml:"file_content"`
	DiffContent  StrategyConfig `yaml:"diff_content"`
}

type StrategyConfig struct {
	DefaultStrategy string `yaml:"default_strategy"`
}

func DefaultRules() *FilterRules {
	return &FilterRules{
		DefaultStrategy: "redact",
		PartStrategies: PartStrategiesConfig{
			Text: TextStrategyConfig{DefaultStrategy: "redact"},
			Tool: ToolStrategyConfig{
				DefaultStrategy: "redact",
				ToolOverrides:   map[string]string{"bash": "redact"},
			},
			Runtime: RuntimeStrategyConfig{
				FileContent: StrategyConfig{DefaultStrategy: "redact"},
				DiffContent: StrategyConfig{DefaultStrategy: "redact"},
			},
		},
		ShellCharThreshold:    120,
		ReasoningThreshold:    -1,
		ContentLengthThreshold: -1,
		PreserveLanguageHint:  true,
		PreserveFilePaths:     true,
		RedactPlaceholder:     "[code filtered]",
	}
}
