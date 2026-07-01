package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// discordChannel posts a plain message to a Discord incoming webhook. No library
// needed — a webhook accepts a JSON body with a "content" field.
type discordChannel struct {
	webhookURL string
}

func (d *discordChannel) name() string { return "discord" }

func (d *discordChannel) send(ctx context.Context, subject, body string) error {
	// Wrap the body in a code block so the aligned columns render monospaced; lead
	// with the subject as bold text.
	content := fmt.Sprintf("**%s**\n```\n%s```", subject, body)
	payload, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Discord returns 204 No Content on success.
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("discord webhook status %d: %s", resp.StatusCode, msg)
	}
	return nil
}
