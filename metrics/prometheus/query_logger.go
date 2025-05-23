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
	"context"
	"log/slog"
)

type QueryLogger struct {
	slog.Handler
}

func (l QueryLogger) Handle(ctx context.Context, record slog.Record) error {
	// Log queries at the debug level.
	record.Level = slog.LevelDebug
	return l.Handler.Handle(ctx, record) //nolint:wrapcheck // We only overwrite this to change the log level, not wrap the error.
}

func (QueryLogger) Close() error { return nil }
