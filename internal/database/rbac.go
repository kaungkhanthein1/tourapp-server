package database

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PromoteAdmins grants role='admin' to every address in the ADMIN_EMAILS
// allow-list (already trimmed/lowercased by config.ParseAdminEmails).
// Idempotent and boot-time only: operators bootstrap the first admin via
// env — never raw SQL. Demotion is deliberately manual
// (UPDATE users SET role = 'user' WHERE ...), so a typo'd env change can
// never silently strip access on restart.
func PromoteAdmins(ctx context.Context, pool *pgxpool.Pool, emails []string) error {
	if len(emails) == 0 {
		return nil
	}
	tag, err := pool.Exec(ctx,
		`UPDATE users SET role = 'admin' WHERE lower(email) = ANY($1)`, emails)
	if err != nil {
		return fmt.Errorf("promote admins: %w", err)
	}
	if n := tag.RowsAffected(); n > 0 {
		log.Printf("[rbac] promoted %d user(s) to admin from ADMIN_EMAILS", n)
	}
	return nil
}
