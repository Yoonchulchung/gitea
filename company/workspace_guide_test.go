// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package company

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPlainFromHTML(t *testing.T) {
	assert.Equal(t, "Use `sqlite3.connect(os.environ[\"DB_PATH\"])` and <b> is gone.",
		plainFromHTML(`Use <code>sqlite3.connect(os.environ[&#34;DB_PATH&#34;])</code> and &lt;b&gt; is <b>gone</b>.`))
}
