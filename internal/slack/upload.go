// Package slack uploads the rendered PDF to a target Slack channel using the
// v2 external-upload flow:
//
//	1. files.getUploadURLExternal returns a one-shot upload URL + file_id.
//	2. PUT the binary to that URL.
//	3. files.completeUploadExternal attaches it to the channel with a comment.
//
// The legacy files.upload endpoint has been deprecated by Slack and will be
// removed.
package slack

import (
	"bytes"
	"context"
	"fmt"

	slacksdk "github.com/slack-go/slack"
)

// Uploader posts PDFs to Slack.
type Uploader struct {
	Token   string
	Channel string // channel ID (preferred — survives renames) or name
}

// New returns an Uploader. Both token and channel are required.
func New(token, channel string) *Uploader {
	return &Uploader{Token: token, Channel: channel}
}

// Upload sends `pdf` as `filename` to the configured channel.
func (u *Uploader) Upload(ctx context.Context, pdf []byte, filename, title, comment string) error {
	if u.Token == "" {
		return fmt.Errorf("slack: empty token")
	}
	if u.Channel == "" {
		return fmt.Errorf("slack: empty channel")
	}
	api := slacksdk.New(u.Token)

	// 1) request an external upload URL.
	urlResp, err := api.GetUploadURLExternalContext(ctx, slacksdk.GetUploadURLExternalParameters{
		FileName: filename,
		FileSize: len(pdf),
	})
	if err != nil {
		return fmt.Errorf("slack: get upload URL: %w", err)
	}

	// 2) PUT the bytes to the returned URL.
	if err := api.UploadToURL(ctx, slacksdk.UploadToURLParameters{
		UploadURL: urlResp.UploadURL,
		Reader:    bytes.NewReader(pdf),
		Filename:  filename,
	}); err != nil {
		return fmt.Errorf("slack: upload to URL: %w", err)
	}

	// 3) complete and share to the channel.
	_, err = api.CompleteUploadExternalContext(ctx, slacksdk.CompleteUploadExternalParameters{
		Files: []slacksdk.FileSummary{{
			ID:    urlResp.FileID,
			Title: title,
		}},
		Channel:        u.Channel,
		InitialComment: comment,
	})
	if err != nil {
		return fmt.Errorf("slack: complete upload: %w", err)
	}
	return nil
}
