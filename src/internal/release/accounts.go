package release

import (
	"fmt"
	"strings"
)

// Default MySQL credentials. A release that does not override them behaves
// exactly like every build made before the settings became editable.
const (
	DefaultRootUser     = "root"
	DefaultRootPassword = "1234"
	DefaultBotUser      = "bot_writer"
	DefaultBotPassword  = "1234"
	DatabaseName        = "jxaccount"
)

// reservedBotUserPrefix is the prefix of the short lived accounts the importer
// creates and drops. A bot account sharing it could be mistaken for one.
const reservedBotUserPrefix = "ktf"

// Accounts are the MySQL credentials baked into a release.
//
// They are recorded in the manifest rather than compiled in, so the operator
// can set them in the Builder and Setup applies exactly what was packaged. The
// root account name itself is not configurable: MySQL's superuser is root, and
// renaming it is a different operation from setting its password.
type Accounts struct {
	RootPassword string `json:"rootPassword"`
	BotUser      string `json:"botUser"`
	BotPassword  string `json:"botPassword"`
}

// DefaultAccounts returns the credentials used when the operator changes
// nothing.
func DefaultAccounts() Accounts {
	return Accounts{
		RootPassword: DefaultRootPassword,
		BotUser:      DefaultBotUser,
		BotPassword:  DefaultBotPassword,
	}
}

// RootAccount is the fixed superuser name.
func (a Accounts) RootAccount() string { return DefaultRootUser }

// WithDefaults fills blank fields, so an empty struct is a valid request for
// the stock credentials.
func (a Accounts) WithDefaults() Accounts {
	if strings.TrimSpace(a.RootPassword) == "" {
		a.RootPassword = DefaultRootPassword
	}
	if strings.TrimSpace(a.BotUser) == "" {
		a.BotUser = DefaultBotUser
	}
	if strings.TrimSpace(a.BotPassword) == "" {
		a.BotPassword = DefaultBotPassword
	}
	return a
}

// Validate rejects credentials that cannot be carried safely through every
// place they are used.
//
// The values end up inside MySQL string literals, a my.ini file, a dotenv file
// and process environments. Only sqlLiteral's single quote doubling protects
// the SQL side, and MySQL treats a backslash as an escape character by default,
// so the character set is restricted rather than escaped per destination.
func (a Accounts) Validate() error {
	if err := validateSecret("MySQL root password", a.RootPassword); err != nil {
		return err
	}
	if err := validateSecret("bot password", a.BotPassword); err != nil {
		return err
	}
	return validateAccountName(a.BotUser)
}

func validateSecret(label, value string) error {
	if value == "" {
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > 32 {
		return fmt.Errorf("%s must be at most 32 characters", label)
	}
	for _, r := range value {
		if r < '!' || r > '~' {
			return fmt.Errorf("%s must use printable ASCII without spaces", label)
		}
		if strings.ContainsRune(`'"`+"`"+`\`, r) {
			return fmt.Errorf("%s must not contain a quote or a backslash", label)
		}
	}
	return nil
}

func validateAccountName(value string) error {
	if value == "" {
		return fmt.Errorf("bot user is required")
	}
	if len(value) > 16 {
		// MySQL 5.5 truncates user names longer than 16 characters, which would
		// silently create an account the bot cannot sign in as.
		return fmt.Errorf("bot user must be at most 16 characters on MySQL 5.5")
	}
	for i, r := range value {
		letter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		digit := r >= '0' && r <= '9'
		switch {
		case letter || r == '_':
		case digit && i > 0:
		default:
			return fmt.Errorf("bot user must be letters, digits and underscore, and must not start with a digit")
		}
	}
	if strings.EqualFold(value, DefaultRootUser) {
		return fmt.Errorf("bot user must not be %q", DefaultRootUser)
	}
	if strings.HasPrefix(strings.ToLower(value), reservedBotUserPrefix) {
		return fmt.Errorf("bot user must not start with %q, which is reserved for temporary import accounts", reservedBotUserPrefix)
	}
	return nil
}
