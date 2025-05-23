// Copyright © 2025 Meroxa, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prometheus

import (
	"fmt"
	"net/url"
	"time"

	"github.com/go-viper/mapstructure/v2"
)

var defaultConfig = Config{
	ScrapeInterval: time.Second,
}

type Config struct {
	// URL points to the metrics endpoint of the service to be monitored.
	URL string `yaml:"url"`
	// ScrapeInterval is the time between scrapes (defaults to 1s).
	ScrapeInterval time.Duration `yaml:"scrape-interval"`
	// Queries are the query configurations used when querying the collected
	// metrics.
	Queries []QueryConfig `yaml:"queries"`
}

func parseConfig(settings map[string]any) (Config, error) {
	cfg := defaultConfig
	dec, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		DecodeHook:       mapstructure.StringToTimeDurationHookFunc(),
		ErrorUnused:      true,
		WeaklyTypedInput: true,
		Result:           &cfg,
		TagName:          "yaml",
	})
	if err != nil {
		return Config{}, fmt.Errorf("failed to create decoder: %w", err)
	}

	err = dec.Decode(settings)
	if err != nil {
		return Config{}, fmt.Errorf("failed to decode settings: %w", err)
	}

	// Try parsing the URL to ensure it's valid.
	_, err = cfg.parseURL()
	if err != nil {
		return Config{}, fmt.Errorf("failed to parse URL: %w", err)
	}

	return cfg, nil
}

func (c Config) parseURL() (*url.URL, error) {
	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}
	return u, nil
}

type QueryConfig struct {
	Name string `yaml:"name"`
	// QueryString is the PromQL query string to be executed.
	QueryString string `yaml:"query"`
	// Interval is the query resolution.
	Interval time.Duration `yaml:"interval"`
	// Unit is the unit of the metric, only for display purposes (optional).
	Unit string `yaml:"unit"`
}
