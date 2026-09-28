---
title: "Notifications"
description: "Telegram notifications for copy"
---

# Notifications

The `copy` command can send start, progress and final notifications to Telegram.
Enable them with `--notify PROFILE`. Repeat the flag
to notify multiple profiles:

```sh
rclone copy source:path destination:path --notify telegram-me --notify telegram-team
```

Without `--notify`, no notification requests are made.

## Profiles

Run `rclone config` and choose `o) Manage notifications`. Choose `n) New
notification profile`, enter a name such as `telegram-me`, and select Telegram.
The wizard asks for the bot token, chat ID and an optional label, then asks you
to confirm saving. Token input is hidden in an interactive terminal and tokens
are saved in obscured form. Creating a profile does not send a message or enable
notifications automatically; select it with `--notify telegram-me` when running
`copy`.

The notification menu also lists profiles and lets you edit or delete them.
While editing, press Enter to keep a value; enter `-` to clear the optional
label. The token is never displayed in the wizard. Declining the save or delete
confirmation leaves the profile unchanged.

You can also find your configuration file with `rclone config file` and edit
notification sections manually. Each section starts with `notify:` followed
by the profile name:

```ini
[notify:telegram-me]
provider = telegram
token = 123456:REPLACE_WITH_BOT_TOKEN
chat_id = 123456789
label = Nightly backup

[notify:telegram-team]
provider = telegram
token = 123456:REPLACE_WITH_BOT_TOKEN
chat_id = -1001234567890
```

The `provider`, `token` and `chat_id` fields are required. Telegram is currently
the only supported provider. The optional `label` is displayed as `Profile` in
the message; it can describe the operation in your own words.

Use the profile name without `notify:` in the flag. Names follow the same rules
as remote names. Repeating the same profile selects it only once. Each selected
profile has its own Telegram session, even when profiles share a bot token.

Notification profiles are excluded from `listremotes`, remote completion and
the interactive remote editor. They are preserved when the configuration is
saved. `rclone config redacted` hides their settings except `provider`.
As with other credentials, `rclone config show` and `rclone config dump` expose
the stored values.

Tokens may be stored as plain text or as `obscured:VALUE`, where `VALUE` is the
output of `rclone obscure`. Obscuring is reversible and is not encryption;
configuration file encryption also applies to notification profiles.

If a profile is missing or invalid, rclone logs the problem and skips that
profile. Other profiles and the file operation continue.

## Timing

Run `rclone config`, choose `o) Manage notifications`, then
`t) Notification timing`. Set the progress interval, request timeout and final
delivery timeout, then confirm saving. These settings apply to all notification
profiles. They are configured through the menu, without command-line flags.

The defaults are 5 seconds between progress snapshots, 10 seconds for each
request and 15 seconds for final delivery to all profiles. Enter a positive
duration such as `500ms`, `30s` or `1m`, or press Enter to keep the displayed
value. Existing configurations without saved timing settings use the defaults.
To disable notifications, omit `--notify`.

Requests run in the background while files are transferred. If delivery is
slow, a new progress snapshot replaces the pending one. Final delivery is
separate and takes priority over pending progress. It gets at most three
attempts, one second apart, within the shutdown timeout. A request already in
progress also consumes that shutdown time.

## Results

One notification session covers all high-level retries of the file operation.
The final message uses the final operation error: an earlier failed attempt
does not turn a successful retry into a failure. Notification errors are logged
without changing the operation's error count or exit status.

Telegram creates a message first and edits that message for progress and the
final result. If Telegram accepts creation but its response is lost, a retry
can create a duplicate message.

Messages include source and destination, transferred bytes, completed transfers
and checks, and deletions when present. The final message includes operation
duration, average speed, and the maximum speed observed in periodic snapshots.
Short speed peaks between snapshots may not be observed. Waiting for final
delivery does not increase the reported operation duration.

Connection-string parameters are omitted from displayed paths because they can
contain credentials. HTTP request dumps are disabled for the Telegram client
to keep bot tokens out of request logs.

Notifications start after command argument and filesystem setup. Errors during
that setup do not produce a notification. Ctrl+C triggers a best-effort final
notification with a cancellation error, subject to the shutdown timeout.
Forced process termination cannot guarantee final delivery.

The final message describes the transfer result. For example,
`--error-on-no-transfer` may choose a nonzero exit status for a successful
operation that did not need to transfer any files.
