package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/valminhq/valmin/internal/auth"
	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// runAcceptNewKey is `valmind admin accept-new-key --confirm-key-loss`. It adopts the current
// master key, generating a missing key file, after the old one is lost for good.
func runAcceptNewKey(ctx context.Context, args []string, getenv func(string) string) error {
	fs := flag.NewFlagSet("admin accept-new-key", flag.ContinueOnError)
	confirm := fs.Bool("confirm-key-loss", false, "reset every secret the old key sealed")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if !*confirm {
		return errors.New("accept-new-key resets stored server passwords and webhook URLs; " +
			"restore secret.key if you can, otherwise rerun with --confirm-key-loss")
	}

	cfg, db, err := openAdminDB(ctx, getenv)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	keeper, err := crypto.OpenForRecovery(ctx, db, cfg.Secrets.MasterKeyFile, getenv)
	if err != nil {
		return fmt.Errorf("master key: %w", err)
	}
	return acceptNewKey(ctx, db, keeper, os.Stdout)
}

// acceptNewKey resets every stored secret keeper cannot open and writes a fresh key check.
// It changes nothing when the key check already opens, so it is safe to rerun.
func acceptNewKey(ctx context.Context, db *store.DB, keeper *crypto.Keeper, out io.Writer) error {
	found, err := keeper.CheckKey(ctx, db)
	if err != nil && !errors.Is(err, crypto.ErrKeyMismatch) {
		return fmt.Errorf("check master key: %w", err)
	}
	if found && err == nil {
		return writeOut(out, "The master key already matches this database. Nothing changed.\n")
	}

	secrets, err := db.ListSecrets(ctx)
	if err != nil {
		return fmt.Errorf("list stored secrets: %w", err)
	}
	var reset keyReset
	for i := range secrets {
		s := &secrets[i]
		loc := crypto.Location{Table: s.Table, Column: s.Column, RowID: s.RowID}
		if _, err := keeper.Decrypt(crypto.Purpose(s.Purpose), loc, s.Envelope); err == nil {
			continue
		}
		if err := reset.secret(ctx, db, keeper, s); err != nil {
			return err
		}
	}
	if err := keeper.WriteKeyCheck(ctx, db); err != nil {
		return fmt.Errorf("accept master key: %w", err)
	}
	return printAccepted(out, &reset)
}

// keyReset collects what acceptNewKey reset, for the report.
type keyReset struct {
	servers, webhooks []string
	discord           bool
}

// secret resets one stored secret the new key cannot open.
func (r *keyReset) secret(ctx context.Context, db *store.DB, keeper *crypto.Keeper, s *store.StaleSecret) error {
	switch s.Table + "." + s.Column {
	case "instances.password":
		line, err := resetServerPassword(ctx, db, keeper, s.RowID)
		if err != nil {
			return err
		}
		r.servers = append(r.servers, line)
	case "instances.rcon_password":
		// Rebuilt from the plugin's config file before the next command.
		if _, err := db.ReplaceSecret(ctx, s, ""); err != nil {
			return fmt.Errorf("clear RCON password of %s: %w", s.RowID, err)
		}
	case "webhooks.url":
		name, err := disableWebhook(ctx, db, s)
		if err != nil {
			return err
		}
		r.webhooks = append(r.webhooks, name)
	case "discord_bot.token":
		if err := disableDiscordBot(ctx, db, s); err != nil {
			return err
		}
		r.discord = true
	}
	return nil
}

// resetServerPassword seals a new game password for one instance and returns the line that
// reports it.
func resetServerPassword(ctx context.Context, db *store.DB, keeper *crypto.Keeper, id string) (string, error) {
	inst, err := db.InstanceByID(ctx, id)
	if err != nil {
		return "", fmt.Errorf("read instance %s: %w", id, err)
	}
	if inst == nil {
		return "", fmt.Errorf("read instance %s: %w", id, store.ErrInstanceNotFound)
	}
	password := auth.RandomPassword()
	for len(instance.ValidateLaunch(inst.ServerName, inst.WorldName, password)) > 0 {
		password = auth.RandomPassword()
	}
	sealed, err := keeper.Encrypt(crypto.PurposeInstancePassword, crypto.InstancePasswordLocation(id), []byte(password))
	if err != nil {
		return "", fmt.Errorf("seal password for %s: %w", inst.Name, err)
	}
	if err := db.ResetInstancePassword(ctx, id, sealed); err != nil {
		return "", fmt.Errorf("store password for %s: %w", inst.Name, err)
	}
	return fmt.Sprintf("    %s: %s", inst.Name, password), nil
}

// disableWebhook turns a destination off before clearing its URL, so a partial run never
// leaves an enabled destination without one.
func disableWebhook(ctx context.Context, db *store.DB, s *store.StaleSecret) (string, error) {
	w, err := db.WebhookByID(ctx, s.RowID)
	if err != nil {
		return "", fmt.Errorf("read webhook %s: %w", s.RowID, err)
	}
	if w == nil {
		return "", fmt.Errorf("webhook %s disappeared during recovery", s.RowID)
	}
	off := false
	if err := db.UpdateWebhook(ctx, s.RowID, nil, nil, &off); err != nil {
		return "", fmt.Errorf("disable webhook %s: %w", w.Name, err)
	}
	if _, err := db.ReplaceSecret(ctx, s, ""); err != nil {
		return "", fmt.Errorf("clear webhook URL of %s: %w", w.Name, err)
	}
	return w.Name, nil
}

// disableDiscordBot turns the bot off before clearing its token.
func disableDiscordBot(ctx context.Context, db *store.DB, s *store.StaleSecret) error {
	if err := db.DisableDiscordBot(ctx); err != nil {
		return fmt.Errorf("disable discord bot: %w", err)
	}
	if _, err := db.ReplaceSecret(ctx, s, ""); err != nil {
		return fmt.Errorf("clear discord bot token: %w", err)
	}
	return nil
}

// printAccepted reports what the recovery reset.
func printAccepted(out io.Writer, r *keyReset) error {
	var b strings.Builder
	b.WriteString("Accepted the new master key.\n")
	if len(r.servers) > 0 {
		b.WriteString("\nServer passwords reset. Give players the new ones; " +
			"the next start recreates the container:\n")
		for _, s := range r.servers {
			b.WriteString(s + "\n")
		}
	}
	if len(r.webhooks) > 0 {
		b.WriteString("\nWebhook destinations disabled. Send each URL again with PATCH /api/v1/admin/webhooks/{id}, " +
			"or delete and re-add the destination on the Notifications page:\n")
		for _, w := range r.webhooks {
			b.WriteString("    " + w + "\n")
		}
	}
	if r.discord {
		b.WriteString("\nDiscord bot disabled. Paste its token again on the Discord page.\n")
	}
	b.WriteString("\nRCON passwords are read again from each server's plugin config. " +
		"Sessions stay valid; reload open panel tabs, since CSRF tokens derive from the key.\n")
	return writeOut(out, b.String())
}

// writeOut writes the command's report.
func writeOut(out io.Writer, s string) error {
	if _, err := io.WriteString(out, s); err != nil {
		return fmt.Errorf("print report: %w", err)
	}
	return nil
}

// openAdminDB loads the configuration and opens and migrates the database for an admin verb.
func openAdminDB(ctx context.Context, getenv func(string) string) (*config.Config, *store.DB, error) {
	cfg, err := config.Load(nil, getenv)
	if err != nil {
		return nil, nil, fmt.Errorf("configuration: %w", err)
	}
	slog.SetDefault(cfg.Log.Logger(os.Stderr))

	db, err := store.Open(ctx, cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		return nil, nil, fmt.Errorf("database %s: %w", cfg.DB.DSN, err)
	}
	if err := migrate(ctx, cfg, db); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return cfg, db, nil
}
