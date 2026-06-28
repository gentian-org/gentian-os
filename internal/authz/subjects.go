// Copyright 2026 The Gentian Authors. Licensed under Apache 2.0.

package authz

import "strings"

// UserSubject formats an OpenFGA user id (user:<id>).
func UserSubject(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "user:"
	}
	if strings.Contains(id, ":") {
		return id
	}
	return "user:" + id
}

// ObjectRef formats an OpenFGA object reference (type:id).
func ObjectRef(objectType, id string) string {
	return objectType + ":" + id
}
