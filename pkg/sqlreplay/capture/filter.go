// Copyright 2024 PingCAP, Inc.
// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"github.com/pingcap/tiproxy/pkg/util/lex"
)

var sensitiveKeywords = [][]string{
	// contain passwords
	{
		"CREATE", "USER",
	},
	{
		"ALTER", "USER",
	},
	{
		"SET", "PASSWORD",
	},
	{
		"GRANT",
	},
	// contain cloud storage url
	{
		"BACKUP",
	},
	{
		"RESTORE",
	},
	{
		"IMPORT",
	},
	// not supported yet
	{
		"LOAD", "DATA",
	},
}

func IsSensitiveSQL(sql string) bool {
	lexer := lex.NewLexer(sql)
	keyword := lexer.NextToken()
	if len(keyword) == 0 {
		return false
	}
	for _, kw := range sensitiveKeywords {
		if keyword != kw[0] {
			continue
		}
		if len(kw) <= 1 {
			return true
		}
		keyword = lexer.NextToken()
		if keyword == kw[1] {
			return true
		}
	}
	return false
}
