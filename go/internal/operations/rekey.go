package operations

import (
	"fmt"
	"os"
	"path/filepath"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// RekeyCredentials re-encrypts the stored credentials from the current
// credential encryption key to next. It holds the state lock, so it refuses
// to run while a gateway serves the state: one that kept running would seal
// new values with a key the database no longer records.
func RekeyCredentials(current, next string) ([]iam.RekeyCount, error) {
	lock, err := AcquireStateLock()
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	if err := RecoverInterruptedRestore(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(config.StateDir(), "gateway.db")); os.IsNotExist(err) {
		return nil, fmt.Errorf("gateway state database does not exist")
	} else if err != nil {
		return nil, err
	}
	counts, err := iam.RekeyCredentials(current, next)
	// Closing checkpoints the write-ahead log into gateway.db before the
	// lock is released.
	if closeErr := iam.Close(); err == nil && closeErr != nil {
		return counts, fmt.Errorf("re-encrypted the credentials, but closing the database failed: %w", closeErr)
	}
	return counts, err
}
