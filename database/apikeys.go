package database

import (
	"context"
	"encoding/base64"
	"time"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

/**
API keys are authentication tokens generated by users. They do not expire, thus
allowing users to use them for a long time and to embed them in apps and on
machines. API keys can be revoked when they are no longer needed or if they get
compromised or are no longer needed. This is done by deleting them from this
service.

There are two kinds of API keys - public and private. We differentiate between
them by the `public` flag.

Private API keys give full API access - using them is equivalent to using a JWT
token, either via an authorization header or a cookie.

Public API keys can only be use for downloading skylinks. The list of skylinks
that can be downloaded by a given public API key is stored under the `skylinks`
array within the API key record.
*/

var (
	// MaxNumAPIKeysPerUser sets the limit for number of API keys a single user
	// can create. If a user reaches that limit they can always delete some API
	// keys in order to make space for new ones. This value is configurable via
	// the ACCOUNTS_MAX_NUM_API_KEYS_PER_USER environment variable.
	MaxNumAPIKeysPerUser = 1000
	// ErrMaxNumAPIKeysExceeded is returned when a user tries to create a new
	// API key after already having the maximum allowed number.
	ErrMaxNumAPIKeysExceeded = errors.New("maximum number of api keys exceeded")
	// ErrInvalidAPIKeyOperation covers a range of invalid operations on API
	// keys. Some examples include: defining a list of skylinks on a private
	// API key, editing a private API key. This error should be used with
	// additional context, specifying the exact operation that failed.
	ErrInvalidAPIKeyOperation = errors.New("invalid api key operation")
)

type (
	// APIKey is a base64URL-encoded representation of []byte with length PubKeySize
	APIKey string
	// APIKeyRecord is a non-expiring authentication token generated on user
	// demand. Public API keys allow downloading a given set of skylinks, while
	// private API keys give full API access.
	APIKeyRecord struct {
		ID        primitive.ObjectID `bson:"_id,omitempty" json:"id"`
		UserID    primitive.ObjectID `bson:"user_id" json:"-"`
		Public    bool               `bson:"public,string" json:"public,string"`
		Key       APIKey             `bson:"key" json:"-"`
		Skylinks  []string           `bson:"skylinks" json:"skylinks"`
		CreatedAt time.Time          `bson:"created_at" json:"createdAt"`
	}
)

// IsValid checks whether the underlying string satisfies the type's requirement
// to represent a []byte with length PubKeySize which is encoded as base64URL.
// This method does NOT check whether the API key exists in the database.
func (ak APIKey) IsValid() bool {
	b := make([]byte, PubKeySize)
	n, err := base64.URLEncoding.Decode(b, []byte(ak))
	return err == nil && n == PubKeySize
}

// CoversSkylink tells us whether a given API key covers a given skylink.
// Private API keys cover all skylinks while public ones - only a limited set.
func (akr APIKeyRecord) CoversSkylink(sl string) bool {
	if akr.Public {
		return true
	}
	for _, s := range akr.Skylinks {
		if s == sl {
			return true
		}
	}
	return false
}

// APIKeyCreate creates a new API key.
func (db *DB) APIKeyCreate(ctx context.Context, user User, public bool, skylinks []string) (*APIKeyRecord, error) {
	if user.ID.IsZero() {
		return nil, errors.New("invalid user")
	}
	n, err := db.staticAPIKeys.CountDocuments(ctx, bson.M{"user_id": user.ID})
	if err != nil {
		return nil, errors.AddContext(err, "failed to ensure user can create a new API key")
	}
	if n > int64(MaxNumAPIKeysPerUser) {
		return nil, ErrMaxNumAPIKeysExceeded
	}
	if !public && len(skylinks) > 0 {
		return nil, errors.AddContext(ErrInvalidAPIKeyOperation, "cannot define skylinks for a private api key")
	}
	ak := APIKeyRecord{
		UserID:    user.ID,
		Public:    public,
		Key:       APIKey(base64.URLEncoding.EncodeToString(fastrand.Bytes(PubKeySize))),
		Skylinks:  skylinks,
		CreatedAt: time.Now().UTC(),
	}
	ior, err := db.staticAPIKeys.InsertOne(ctx, ak)
	if err != nil {
		return nil, err
	}
	ak.ID = ior.InsertedID.(primitive.ObjectID)
	return &ak, nil
}

// APIKeyDelete deletes an API key.
func (db *DB) APIKeyDelete(ctx context.Context, user User, akID primitive.ObjectID) error {
	if user.ID.IsZero() {
		return errors.New("invalid user")
	}
	filter := bson.M{
		"_id":     akID,
		"user_id": user.ID,
	}
	dr, err := db.staticAPIKeys.DeleteOne(ctx, filter)
	if err != nil {
		return err
	}
	if dr.DeletedCount == 0 {
		return mongo.ErrNoDocuments
	}
	return nil
}

// APIKeyByKey returns a specific API key.
func (db *DB) APIKeyByKey(ctx context.Context, key string) (APIKeyRecord, error) {
	filter := bson.M{"key": key}
	sr := db.staticAPIKeys.FindOne(ctx, filter)
	if sr.Err() != nil {
		return APIKeyRecord{}, sr.Err()
	}
	var akr APIKeyRecord
	err := sr.Decode(&akr)
	if err != nil {
		return APIKeyRecord{}, err
	}
	return akr, nil
}

// APIKeyGet returns a specific API key.
func (db *DB) APIKeyGet(ctx context.Context, akID primitive.ObjectID) (APIKeyRecord, error) {
	filter := bson.M{"_id": akID}
	sr := db.staticAPIKeys.FindOne(ctx, filter)
	if sr.Err() != nil {
		return APIKeyRecord{}, sr.Err()
	}
	var akr APIKeyRecord
	err := sr.Decode(&akr)
	if err != nil {
		return APIKeyRecord{}, err
	}
	return akr, nil
}

// APIKeyList lists all API keys that belong to the user.
func (db *DB) APIKeyList(ctx context.Context, user User) ([]APIKeyRecord, error) {
	if user.ID.IsZero() {
		return nil, errors.New("invalid user")
	}
	c, err := db.staticAPIKeys.Find(ctx, bson.M{"user_id": user.ID})
	if err != nil {
		return nil, err
	}
	// We want this to be a make in order to make sure its JSON representation
	// is a valid JSONArray and not a null.
	aks := make([]APIKeyRecord, 0)
	err = c.All(ctx, &aks)
	if err != nil {
		return nil, err
	}
	return aks, nil
}

// APIKeyUpdate updates an existing API key. This works by replacing the
// list of Skylinks within the API key record. Only valid for public API keys.
func (db *DB) APIKeyUpdate(ctx context.Context, user User, akID primitive.ObjectID, skylinks []string) error {
	if user.ID.IsZero() {
		return errors.New("invalid user")
	}
	// Validate all given skylinks.
	for _, s := range skylinks {
		if !ValidSkylinkHash(s) {
			return ErrInvalidSkylink
		}
	}
	filter := bson.M{
		"_id":     akID,
		"public":  &True, // you can only update public API keys
		"user_id": user.ID,
	}
	update := bson.M{"$set": bson.M{"skylinks": skylinks}}
	opts := options.UpdateOptions{
		Upsert: &False,
	}
	_, err := db.staticAPIKeys.UpdateOne(ctx, filter, update, &opts)
	return err
}

// APIKeyPatch updates an existing API key. This works by adding and removing
// skylinks to its record. Only valid for public API keys.
func (db *DB) APIKeyPatch(ctx context.Context, user User, akID primitive.ObjectID, addSkylinks, removeSkylinks []string) error {
	if user.ID.IsZero() {
		return errors.New("invalid user")
	}
	// Validate all given skylinks.
	for _, s := range append(addSkylinks, removeSkylinks...) {
		if !ValidSkylinkHash(s) {
			return ErrInvalidSkylink
		}
	}
	filter := bson.M{
		"_id":    akID,
		"public": &True, // you can only update public API keys
	}
	var update bson.M
	// First, all new skylinks to the record.
	if len(addSkylinks) > 0 {
		update = bson.M{
			"$push": bson.M{"skylinks": bson.M{"$each": addSkylinks}},
		}
		opts := options.UpdateOptions{
			Upsert: &False,
		}
		_, err := db.staticAPIKeys.UpdateOne(ctx, filter, update, &opts)
		if err != nil {
			return err
		}
	}
	// Then, remove all skylinks that need to be removed.
	if len(removeSkylinks) > 0 {
		update = bson.M{
			"$pull": bson.M{"skylinks": bson.M{"$in": removeSkylinks}},
		}
		opts := options.UpdateOptions{
			Upsert: &False,
		}
		_, err := db.staticAPIKeys.UpdateOne(ctx, filter, update, &opts)
		if err != nil {
			return err
		}
	}
	return nil
}
