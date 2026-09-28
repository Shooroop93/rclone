package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/notification"
)

const (
	urlSendMessage = "https://api.telegram.org/bot%s/sendMessage"
	urlEditMessage = "https://api.telegram.org/bot%s/editMessageText"
)

// SendMessageRequest contains the destination and text for a new Telegram message.
type SendMessageRequest struct {
	// ChatID identifies the destination chat.
	ChatID string `json:"chat_id"`
	// Text is the message content.
	Text string `json:"text"`
}

type sendMessageResponse struct {
	OK          bool               `json:"ok"`
	Result      *sendMessageResult `json:"result"`
	Description string             `json:"description"`
	ErrorCode   int                `json:"error_code"`
}

type sendMessageResult struct {
	MessageID int `json:"message_id"`
}

// EditMessageTextRequest contains the message to update and its replacement text.
type EditMessageTextRequest struct {
	// ChatID identifies the destination chat.
	ChatID string `json:"chat_id"`
	// MessageID identifies the message within the chat.
	MessageID int `json:"message_id"`
	// Text is the replacement message content.
	Text string `json:"text"`
}

// Session delivers an operation's notifications in a single Telegram message.
// A Session must not be used concurrently.
type Session struct {
	client    *http.Client
	token     string
	chatID    string
	messageID int
	message   string
}

// NewSession creates a session with a ten-second request timeout.
func NewSession(ctx context.Context, token, chatID string) *Session {
	return NewSessionWithTimeout(ctx, token, chatID, 10*time.Second)
}

// NewSessionWithTimeout creates a session with the specified request timeout.
// Nonpositive timeout values use the default of ten seconds.
func NewSessionWithTimeout(ctx context.Context, token, chatID string, timeout time.Duration) *Session {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, ci := fs.AddConfig(ctx)
	// Telegram includes the bot token in request URLs, which HTTP dumps expose.
	ci.Dump = 0
	client := fshttp.NewClient(ctx)
	client.Timeout = timeout

	return &Session{
		client: client,
		token:  token,
		chatID: chatID,
	}
}

// Notify creates or updates the operation's Telegram message using ctx.
// Identical successful messages are skipped; failed deliveries can be retried.
func (s *Session) Notify(ctx context.Context, state notification.State) error {

	fMessage, err := notification.FormatMessage(state)

	if err != nil {
		return fmt.Errorf("notify telegram formatMessage: %w", err)
	}

	if s.messageID != 0 && s.message == fMessage {
		return nil
	}

	if s.messageID == 0 {
		messageID, err := sendMessage(ctx, s.client, s.token, s.chatID, fMessage)

		if err != nil {
			return fmt.Errorf("notify sendMessageRequest: %w", err)
		}

		s.messageID = messageID
		s.message = fMessage

		return nil
	}

	err = editMessageText(ctx, s.client, s.token, s.chatID, fMessage, s.messageID)

	if err != nil {
		return fmt.Errorf("notify editMessageText: %w", err)
	}

	s.message = fMessage
	return nil
}

func buildEditMessageTextBody(chatID, text string, messageID int) ([]byte, error) {

	request := EditMessageTextRequest{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	}

	body, err := json.Marshal(request)

	if err != nil {
		return nil, fmt.Errorf("failed to marshal telegram edit message request: %w", err)
	}

	return body, nil
}

func buildEditMessageTextRequest(ctx context.Context, token, chatID, text string, messageID int) (*http.Request, error) {

	body, err := buildEditMessageTextBody(chatID, text, messageID)

	if err != nil {
		return nil, fmt.Errorf("failed to build telegram edit message request body: %w", err)
	}

	url := fmt.Sprintf(
		urlEditMessage,
		token,
	)
	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url,
		bytes.NewReader(body),
	)

	if err != nil {
		return nil, fmt.Errorf("failed create new edit message request to telegram: %w", unwrapURLError(err))
	}

	req.Header.Set("Content-Type", "application/json")

	return req, nil
}

func buildSendMessageBody(chatID, text string) ([]byte, error) {

	request := SendMessageRequest{
		ChatID: chatID,
		Text:   text,
	}

	body, err := json.Marshal(request)

	if err != nil {
		return nil, fmt.Errorf("failed to marshal telegram request: %w", err)
	}

	return body, nil
}

func buildSendMessageRequest(ctx context.Context, token, chatID, text string) (*http.Request, error) {

	body, err := buildSendMessageBody(chatID, text)
	if err != nil {
		return nil, fmt.Errorf("failed to build telegram request body: %w", err)
	}

	url := fmt.Sprintf(
		urlSendMessage,
		token,
	)

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		url,
		bytes.NewReader(body),
	)

	if err != nil {
		return nil, fmt.Errorf("failed create new request to telegram: %w", unwrapURLError(err))
	}

	req.Header.Set("Content-Type", "application/json")

	return req, nil
}

func doRequest(client *http.Client, req *http.Request) ([]byte, error) {

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send telegram request: %w", unwrapURLError(err))
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)

	if err != nil {
		return nil, fmt.Errorf("failed to read telegram response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, fmt.Errorf(
			"telegram returned status %s: %s",
			resp.Status,
			body,
		)
	}

	return body, nil
}

func sendMessage(ctx context.Context, client *http.Client, token, chatID, text string) (int, error) {

	req, err := buildSendMessageRequest(ctx, token, chatID, text)

	if err != nil {
		return 0, fmt.Errorf("failed to build send message request: %w", err)
	}

	body, err := doRequest(client, req)

	if err != nil {
		return 0, fmt.Errorf("failed to send telegram message: %w", err)
	}

	messageID, err := parseMessageID(body)
	if err != nil {
		return 0, fmt.Errorf(
			"failed to parse telegram send message response: %w",
			err,
		)
	}

	return messageID, nil
}

func editMessageText(ctx context.Context, client *http.Client, token, chatID, text string, messageID int) error {

	req, err := buildEditMessageTextRequest(ctx, token, chatID, text, messageID)
	if err != nil {
		return fmt.Errorf("failed to build edit message request: %w", err)
	}

	res, err := doRequest(client, req)

	if err != nil {
		return fmt.Errorf("failed to edit telegram message: %w", err)
	}

	var bodyResp sendMessageResponse

	err = json.Unmarshal(res, &bodyResp)

	if err != nil {
		return fmt.Errorf("failed to unmarshal telegram send message response: %w", err)
	}

	if !bodyResp.OK {
		return fmt.Errorf(
			"telegram API error %d: %s",
			bodyResp.ErrorCode,
			bodyResp.Description,
		)
	}

	return nil
}

func parseMessageID(body []byte) (int, error) {
	var entry sendMessageResponse

	if err := json.Unmarshal(body, &entry); err != nil {
		return 0, fmt.Errorf("failed unmarshal parse telegram response: %w", err)
	}

	if !entry.OK {
		return 0, fmt.Errorf(
			"telegram response is not ok",
		)
	}

	if entry.Result == nil {
		return 0, fmt.Errorf(
			"telegram response does not contain result",
		)
	}

	if entry.Result.MessageID == 0 {
		return 0, fmt.Errorf(
			"telegram response does not contain message_id",
		)
	}

	return entry.Result.MessageID, nil
}

func unwrapURLError(err error) error {
	var urlErr *url.Error

	if errors.As(err, &urlErr) {
		if urlErr == nil || urlErr.Err == nil {
			return errors.New("telegram request failed without an error cause")
		}
		return urlErr.Err
	}
	return err
}
