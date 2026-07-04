package users

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// OwnerSpec is the owner identity enforced at boot. Copied from
// config.OwnerConfig at the call site rather than importing config here to
// keep the users→config dependency one-way (config already imports nobody).
type OwnerSpec struct {
	Username    string
	Email       string
	Password    string
	DisplayName string
}

// EnsureSingleOwner enforces the configured single-user identity against the
// store. It is a no-op when owner.Username is empty (no owner.json).
//
// Otherwise it:
//  1. Upserts the owner account — creates it if missing, or rotates the
//     password (and email) to match the config when it already exists.
//  2. For every OTHER super-admin account, migrates its data to the owner
//     (store.ReassignUserData) and then deletes it.
//
// channel_user (chatter) rows are end-user identities on IM channels, not
// owner accounts, so they are left in place. After this runs the users table
// holds exactly one super-admin (the owner) plus whatever chatters exist.
func EnsureSingleOwner(ctx context.Context, st store.Store, owner OwnerSpec) error {
	if strings.TrimSpace(owner.Username) == "" {
		return nil
	}
	accts, err := NewAccounts(st)
	if err != nil {
		return err
	}

	// 1. Upsert owner.
	var ownerID string
	if rec, err := st.GetUserByLogin(ctx, owner.Username); err == nil {
		ownerID = rec.ID
		if owner.Password != "" {
			if err := accts.SetPassword(ctx, rec.ID, owner.Password); err != nil {
				return err
			}
		}
		if owner.Email != "" && owner.Email != rec.Email {
			rec.Email = strings.ToLower(strings.TrimSpace(owner.Email))
			if err := st.UpdateUser(ctx, rec); err != nil {
				return err
			}
		}
	} else if errors.Is(err, store.ErrNotFound) {
		email := owner.Email
		if email == "" {
			email = owner.Username + "@local.fastclaw"
		}
		acct, err := accts.Create(ctx, CreateInput{
			Username:    owner.Username,
			Email:       email,
			Password:    owner.Password,
			DisplayName: owner.DisplayName,
			Role:        RoleSuperAdmin,
		})
		if err != nil {
			return err
		}
		ownerID = acct.ID
	} else {
		return err
	}

	// 2. Merge + drop every other super-admin account.
	list, err := accts.List(ctx)
	if err != nil {
		return err
	}
	for _, a := range list {
		if a.ID == ownerID || a.Role == RoleChannelUser {
			continue
		}
		if err := st.ReassignUserData(ctx, a.ID, ownerID); err != nil {
			return err
		}
		if err := st.DeleteUser(ctx, a.ID); err != nil {
			return err
		}
		slog.Info("single-user: merged surplus account into owner",
			"removed", a.Username, "owner", owner.Username)
	}
	return nil
}
