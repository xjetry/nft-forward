package server

import (
	"database/sql"
	"fmt"

	"nft-forward/internal/db"
)

// ResetAdminPassword rewrites the password of an admin account and
// invalidates all its sessions so any leaked cookie stops working
// immediately. Returns a human-readable status message on success.
func ResetAdminPassword(d *sql.DB, username, newPw string) (string, error) {
	u, err := db.GetUserByUsername(d, username)
	if err != nil {
		return "", fmt.Errorf("user %q not found: %w", username, err)
	}
	if u.Role != "admin" {
		return "", fmt.Errorf("user %q has role %s, not admin", username, u.Role)
	}
	hash, err := HashPassword(newPw)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	if _, err := d.Exec(`UPDATE users SET pw_hash=?, disabled=0 WHERE id=?`, hash, u.ID); err != nil {
		return "", fmt.Errorf("update password: %w", err)
	}
	_, _ = d.Exec(`DELETE FROM sessions WHERE user_id=?`, u.ID)
	db.WriteAudit(d, u.ID, "admin.reset_password_cli", username, "")
	return fmt.Sprintf("password reset for %s (all sessions invalidated, disabled flag cleared)", username), nil
}
