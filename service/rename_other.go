//go:build !linux

package service

func renameNoReplace(from, to string) error { return renameChecked(from, to) }
