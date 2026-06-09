package filter

import (
	"os"

	"gopkg.in/yaml.v3"
)

func LoadRules(path string) (*FilterRules, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	rules := DefaultRules()
	if err := yaml.Unmarshal(data, rules); err != nil {
		return nil, err
	}

	return rules, nil
}
