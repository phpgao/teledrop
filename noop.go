package main

import "context"

// NoopUploader is a disabled-state placeholder that does nothing and always succeeds.
type NoopUploader struct{}

func (u *NoopUploader) Upload(_ context.Context, _, _ string, _ bool) error { return nil }
