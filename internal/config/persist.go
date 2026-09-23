package config

import (
	"strconv"
	"strings"
)

// persistSessionPrefix starts every tmux session name relay creates for a
// persist host template, so relay can pick its own out of a host's `tmux ls`.
const persistSessionPrefix = "relay-"

// persistProjectLen is how much of the project id a session name carries:
// the first 8 characters, the leading block of a project's uuid.
const persistProjectLen = 8

// PersistSessionName is the tmux session name for the nth persist launch of
// templateID in projectID: relay-<project8>-<template>-<n>. Every character
// outside [A-Za-z0-9_-] becomes _, which covers the `.` and `:` tmux refuses
// in a session name. ParsePersistSessionName reverses it.
func PersistSessionName(projectID, templateID string, n int) string {
	return persistSessionPrefix + persistProject8(projectID) + "-" + sanitizePersistPart(templateID) + "-" + strconv.Itoa(n)
}

// ParsePersistSessionName recovers the parts of a PersistSessionName: the 8
// characters after relay-, the (sanitized) template id, and n, taken after
// the last -. ok is false for any name PersistSessionName could not have
// produced — a foreign tmux session, not relay's.
func ParsePersistSessionName(name string) (project8, templateID string, n int, ok bool) {
	rest, found := strings.CutPrefix(name, persistSessionPrefix)
	if !found || len(rest) < persistProjectLen+1 || rest[persistProjectLen] != '-' {
		return "", "", 0, false
	}
	project8, rest = rest[:persistProjectLen], rest[persistProjectLen+1:]
	i := strings.LastIndexByte(rest, '-')
	if i <= 0 {
		return "", "", 0, false
	}
	templateID, digits := rest[:i], rest[i+1:]
	n, err := strconv.Atoi(digits)
	// strconv.Itoa(n) == digits refuses the forms Atoi accepts but Itoa never
	// writes: a sign, leading zeros.
	if err != nil || n <= 0 || strconv.Itoa(n) != digits {
		return "", "", 0, false
	}
	if !isPersistSafe(project8) || !isPersistSafe(templateID) {
		return "", "", 0, false
	}
	return project8, templateID, n, true
}

// ParseProjectPersistSessionName is ParsePersistSessionName narrowed to one
// project: ok only when name parses and its prefix is projectID's, derived
// exactly as PersistSessionName derives it. The prefix is 8 characters, so
// two projects whose ids share them are indistinguishable here; project ids
// are uuids, which keeps that from arising.
func ParseProjectPersistSessionName(name, projectID string) (templateID string, n int, ok bool) {
	project8, templateID, n, ok := ParsePersistSessionName(name)
	if !ok || project8 != persistProject8(projectID) {
		return "", 0, false
	}
	return templateID, n, true
}

// NextPersistSessionN is the n for a new persist session of templateID in
// projectID: one more than the largest n among existing names that belong to
// that project and template, or 1 when none do.
func NextPersistSessionN(existing []string, projectID, templateID string) int {
	wantProject, wantTemplate := persistProject8(projectID), sanitizePersistPart(templateID)
	highest := 0
	for _, name := range existing {
		p, t, n, ok := ParsePersistSessionName(name)
		if ok && p == wantProject && t == wantTemplate && n > highest {
			highest = n
		}
	}
	return highest + 1
}

// persistProject8 sanitizes before truncating: sanitizing maps every rune to
// one byte, so the cut can never split a rune. A project id of fewer than 8
// characters yields a name ParsePersistSessionName refuses; project ids are
// uuids, so that does not arise.
func persistProject8(projectID string) string {
	s := sanitizePersistPart(projectID)
	if len(s) > persistProjectLen {
		s = s[:persistProjectLen]
	}
	return s
}

func sanitizePersistPart(s string) string {
	return strings.Map(func(r rune) rune {
		if isPersistSafeRune(r) {
			return r
		}
		return '_'
	}, s)
}

func isPersistSafe(s string) bool {
	for _, r := range s {
		if !isPersistSafeRune(r) {
			return false
		}
	}
	return true
}

func isPersistSafeRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}
