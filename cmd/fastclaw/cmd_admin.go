package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// adminCmd groups the admin-only CLI operations. Single-user mode keeps
// just create-user (bootstrap the owner) and reset-password (lockout
// recovery). These bypass the HTTP API and write to the DB directly.
func adminCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administrative DB operations (create owner, reset password)",
	}
	cmd.AddCommand(adminCreateUserCmd())
	cmd.AddCommand(adminResetPasswordCmd())
	return cmd
}

func openStoreFromEnv() (store.Store, error) {
	env := config.LoadEnv()
	homeDir, _ := config.HomeDir()
	return store.New(&store.StorageConfig{
		Type:        store.StorageType(env.Storage.Type),
		DSN:         env.Storage.DSN,
		AutoMigrate: true,
	}, homeDir)
}

func adminCreateUserCmd() *cobra.Command {
	var username, email, password, displayName string
	cmd := &cobra.Command{
		Use:   "create-user",
		Short: "Create the owner account (single-user mode: always super_admin)",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			accts, err := users.NewAccounts(st)
			if err != nil {
				return err
			}
			acct, err := accts.Create(context.Background(), users.CreateInput{
				Username:    username,
				Email:       email,
				Password:    password,
				DisplayName: displayName,
				Role:        users.RoleSuperAdmin,
			})
			if err != nil {
				return err
			}
			fmt.Printf("created owner %s (%s) id=%s\n", acct.Username, acct.Email, acct.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "username (required)")
	cmd.Flags().StringVar(&email, "email", "", "email (required)")
	cmd.Flags().StringVar(&password, "password", "", "password (required)")
	cmd.Flags().StringVar(&displayName, "display-name", "", "display name")
	cmd.MarkFlagRequired("username")
	cmd.MarkFlagRequired("email")
	cmd.MarkFlagRequired("password")
	return cmd
}

func adminResetPasswordCmd() *cobra.Command {
	var login, password string
	cmd := &cobra.Command{
		Use:   "reset-password",
		Short: "Reset the owner's password by username or email",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStoreFromEnv()
			if err != nil {
				return err
			}
			defer st.Close()
			accts, _ := users.NewAccounts(st)
			rec, err := st.GetUserByLogin(context.Background(), login)
			if err != nil {
				return err
			}
			if err := accts.SetPassword(context.Background(), rec.ID, password); err != nil {
				return err
			}
			fmt.Printf("password reset for %s\n", rec.Username)
			return nil
		},
	}
	cmd.Flags().StringVar(&login, "user", "", "username or email (required)")
	cmd.Flags().StringVar(&password, "password", "", "new password (required)")
	cmd.MarkFlagRequired("user")
	cmd.MarkFlagRequired("password")
	return cmd
}
