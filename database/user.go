package database

import (
	"os"
	"regexp"

	"gitlab.com/NebulousLabs/errors"

	"github.com/NebulousLabs/skynet-accounts/build"
	"github.com/NebulousLabs/skynet-accounts/lib"

	"gitlab.com/NebulousLabs/fastrand"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"golang.org/x/crypto/bcrypt"
)

// User status tiers.
const (
	TierUnconfirmed = iota
	TierFree
	TierPremium
)

var (
	// ErrInvalidEmail is returned when we encounter an invalid email value.
	ErrInvalidEmail = errors.New("invalid email")

	// emailValidatorRegEx checks if a string is a valid email address.
	// See https://emailregex.com/
	emailValidatorRegEx = regexp.MustCompile(`^[A-Z0-9a-z._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,64}$`)

	// envPepper holds the name of the environment variable for password pepper.
	// This is not in main in order to avoid a circular dependency.
	envPepper = "SKYNET_PEPPER"

	// pepper is the system's password pepper.
	pepper = []byte{}

	// saltSize specifies the length of the Salt []byte
	saltSize = 32
)

type (
	// Email is an email.
	Email string

	// User represents a Skynet user.
	User struct {
		// ID is a hexadecimal string representation of the MongoDB id assigned
		// to this user object. It is auto-generated by Mongo on insert.
		ID        primitive.ObjectID `bson:"_id,omitempty" json:"-"`
		FirstName string             `bson:"firstName" json:"firstName"`
		LastName  string             `bson:"lastName" json:"lastName"`
		Email     Email              `bson:"email" json:"email"`
		Tier      int                `bson:"tier" json:"tier"`
		Password  []byte             `bson:"password" json:"-"`
		Salt      []byte             `bson:"salt" json:"-"`

		dep lib.Dependencies
	}
)

// NewEmail returns a new valid Email instance or an error.
func NewEmail(email string) (Email, error) {
	em := Email(email)
	if !em.Validate() {
		return "", ErrInvalidEmail
	}
	return em, nil
}

// Validate validates an email address.
func (e Email) Validate() bool {
	return emailValidatorRegEx.MatchString(string(e))
}

// VerifyPassword verifies that the given password is correct for this user.
func (u *User) VerifyPassword(pw string) error {
	return bcrypt.CompareHashAndPassword(u.Password, append([]byte(pw), u.saltAndPepper()...))
}

// SetPassword sets the user's password.
func (u *User) SetPassword(pw string) (err error) {
	oldSalt := u.Salt
	defer func() {
		if err != nil {
			u.Salt = oldSalt
		}
	}()
	u.Salt = fastrand.Bytes(saltSize)
	pwHash, err := bcrypt.GenerateFromPassword(append([]byte(pw), u.saltAndPepper()...), bcrypt.DefaultCost)
	if u.dep != nil && u.dep.Disrupt("DependencyHashPassword") {
		err = errors.Compose(err, errors.New("DependencyHashPassword"))
	}
	if err != nil {
		return err
	}
	u.Password = pwHash
	return nil
}

// saltAndPepper is a convenience function that returns the user's salt and the
// system's pepper in a single slice.
func (u *User) saltAndPepper() []byte {
	if len(pepper) == 0 {
		pv, ok := os.LookupEnv(envPepper)
		if !ok && build.Release != "testing" {
			build.Severe("Failed to load the password pepper! Using un-peppered passwords!")
		}
		pepper = []byte(pv)
	}
	return append(u.Salt, pepper...)
}
