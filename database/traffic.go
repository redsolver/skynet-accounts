package database

import (
	"context"
	"sync"
	"time"

	"github.com/NebulousLabs/skynet-accounts/skynet"

	"gitlab.com/NebulousLabs/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

type Traffic struct {
	DownloadCount     int   `json:"downloadCount"`
	DownloadSize      int64 `json:"downloadSize"`
	DownloadBandwidth int64 `json:"downloadBandwidth"`
	UploadCount       int   `json:"uploadCount"`
	UploadSize        int64 `json:"uploadSize"`
	UploadBandwidth   int64 `json:"uploadBandwidth"`
	RegistryReads     int   `json:"registryReads"`
	RegistryWrites    int   `json:"registryWrites"`
}

type TrafficDTO struct {
	Source      string  `json:"source"`
	SourceType  string  `json:"source_type"`
	Total       Traffic `json:"total"`
	Last24Hours Traffic `json:"last24hours"`
}

// trafficStats describes a given type of traffic, e.g. upload or download
type trafficStats struct {
	CountTotal        int
	Count24Hours      int
	BandwidthPeriod   int64
	Bandwidth24Hours  int64
	UploadSizePeriod  int64
	UploadSize24Hours int64
}

//// TrafficByTopReferrers ...
//func (db *DB) TrafficByTopReferrers(ctx context.Context, user User, offset, pageSize int) ([]TrafficDTO, int, error) {
//	if user.ID.IsZero() {
//		return nil, 0, errors.New("invalid user")
//	}
//	if err := validateOffsetPageSize(offset, pageSize); err != nil {
//		return nil, 0, err
//	}
//
//	matchStage := bson.D{{"$match", bson.D{
//		{"user_id", user.ID},
//		{"unpinned", false},
//	}}}
//
//	ref, err := FromString(referrer)
//
//	if err == nil {
//		matchStage = bson.D{{"$match", bson.D{
//			{"user_id", user.ID},
//			{"unpinned", false},
//			{"referrer", ref.CanonicalName},
//			//{"referrer_type", ref.Type}, // TODO Not sure if we this would be useful in any way
//		}}}
//	}
//	return db.uploadsBy(ctx, matchStage, offset, pageSize)
//}

func (db *DB) UserTraffic(ctx context.Context, user User, startOfPeriod time.Time) (map[Referrer]TrafficDTO, error) {
	return db.userTraffic(ctx, user, startOfPeriod)
}

// userStats reports statistical information about the user.
func (db *DB) userTraffic(ctx context.Context, user User, startOfPeriod time.Time) (map[Referrer]TrafficDTO, error) {
	var errs []error
	var errsMux sync.Mutex
	regErr := func(msg string, e error) {
		db.staticLogger.Info(msg, e)
		errsMux.Lock()
		errs = append(errs, e)
		errsMux.Unlock()
	}

	traffic := make(map[Referrer]TrafficDTO)
	var trafficMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(1)
	// Uploads
	go func() {
		defer wg.Done()
		tm, err := db.userUploadTraffic(ctx, user.ID, startOfPeriod)
		if err != nil {
			regErr("Failed to get user's upload traffic:", err)
			return
		}
		trafficMu.Lock()
		for r, t := range tm {
			rt, exists := traffic[r]
			if !exists {
				traffic[r] = TrafficDTO{
					Source:      r.CanonicalName,
					SourceType:  r.Type,
					Total:       Traffic{},
					Last24Hours: Traffic{},
				}
				rt = traffic[r]
			}
			// We increment the bandwidth instead of setting it because
			// registry writes count towards it as well.
			rt.Total.UploadCount = t.CountTotal
			rt.Total.UploadSize = t.UploadSizePeriod
			rt.Total.UploadBandwidth = t.BandwidthPeriod
			rt.Last24Hours.UploadCount = t.Count24Hours
			rt.Last24Hours.UploadSize = t.UploadSize24Hours
			rt.Last24Hours.UploadBandwidth = t.Bandwidth24Hours
		}
		trafficMu.Unlock()
	}()
	wg.Add(1)
	// Downloads
	go func() {
		defer wg.Done()
		tm, err := db.userDownloadTraffic(ctx, user.ID, startOfPeriod)
		if err != nil {
			regErr("Failed to get user's download traffic:", err)
			return
		}
		trafficMu.Lock()
		for r, t := range tm {
			rt, exists := traffic[r]
			if !exists {
				traffic[r] = TrafficDTO{
					Source:      r.CanonicalName,
					SourceType:  r.Type,
					Total:       Traffic{},
					Last24Hours: Traffic{},
				}
				rt = traffic[r]
			}
			// We increment the bandwidth instead of setting it because
			// registry reads count towards it as well.
			rt.Total.DownloadCount = t.CountTotal
			rt.Total.DownloadBandwidth += t.BandwidthPeriod
			rt.Last24Hours.DownloadCount = t.Count24Hours
			rt.Last24Hours.DownloadBandwidth += t.Bandwidth24Hours
		}
		trafficMu.Unlock()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		tm, err := db.userRegistryReadTraffic(ctx, user.ID, startOfPeriod)
		if err != nil {
			regErr("Failed to get user's registry read traffic:", err)
			return
		}
		trafficMu.Lock()
		for r, t := range tm {
			rt, exists := traffic[r]
			if !exists {
				traffic[r] = TrafficDTO{
					Source:      r.CanonicalName,
					SourceType:  r.Type,
					Total:       Traffic{},
					Last24Hours: Traffic{},
				}
				rt = traffic[r]
			}
			rt.Total.RegistryReads = t.CountTotal
			rt.Total.DownloadBandwidth += t.BandwidthPeriod
			rt.Last24Hours.RegistryReads = t.Count24Hours
			rt.Last24Hours.DownloadBandwidth += t.Bandwidth24Hours
		}
		trafficMu.Unlock()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		tm, err := db.userRegistryReadTraffic(ctx, user.ID, startOfPeriod) // TODO Change to "Write"
		if err != nil {
			regErr("Failed to get user's registry read traffic:", err)
			return
		}
		trafficMu.Lock()
		for r, t := range tm {
			rt, exists := traffic[r]
			if !exists {
				traffic[r] = TrafficDTO{
					Source:      r.CanonicalName,
					SourceType:  r.Type,
					Total:       Traffic{},
					Last24Hours: Traffic{},
				}
				rt = traffic[r]
			}
			rt.Total.RegistryWrites = t.CountTotal
			rt.Total.UploadBandwidth += t.BandwidthPeriod
			rt.Last24Hours.RegistryWrites = t.Count24Hours
			rt.Last24Hours.UploadBandwidth += t.Bandwidth24Hours
		}
		trafficMu.Unlock()
	}()

	wg.Wait()
	if len(errs) > 0 {
		return nil, errors.Compose(errs...)
	}
	return traffic, nil
}

func (db *DB) userUploadTraffic(ctx context.Context, userID primitive.ObjectID, periodStart time.Time) (map[Referrer]trafficStats, error) {
	c, err := db.staticUploads.Aggregate(ctx, trafficPipeline(userID, periodStart))
	if err != nil {
		return nil, err
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
		}
	}()
	// We need this struct, so we can safely decode both int32 and int64.
	result := struct {
		Size         int64     `bson:"size"`
		Skylink      string    `bson:"skylink"`
		Unpinned     bool      `bson:"unpinned"`
		Timestamp    time.Time `bson:"timestamp"`
		Referrer     string    `bson:"referrer"`
		ReferrerType string    `bson:"referrer_type"`
	}{}
	processedSkylinks := make(map[string]bool)
	trafficMap := make(map[Referrer]trafficStats)
	var last24 bool
	for c.Next(ctx) {
		if err = c.Decode(&result); err != nil {
			return nil, errors.AddContext(err, "failed to decode DB data")
		}
		ref := Referrer{
			CanonicalName: result.Referrer,
			Type:          result.ReferrerType,
		}
		if _, exists := trafficMap[ref]; !exists {
			trafficMap[ref] = trafficStats{}
		}
		traffic := trafficMap[ref]
		last24 = result.Timestamp.After(time.Now().Add(-1 * time.Hour))
		// All bandwidth is counted, regardless of unpinned status.
		band := skynet.BandwidthUploadCost(result.Size)
		traffic.BandwidthPeriod += band
		if last24 {
			traffic.Bandwidth24Hours += band
		}
		// Count only uploads that are still pinned towards total count.
		if result.Unpinned {
			continue
		}
		traffic.CountTotal++
		if last24 {
			traffic.Count24Hours++
		}
		// Count only unique uploads towards total size and used storage.
		if processedSkylinks[result.Skylink] {
			continue
		}
		processedSkylinks[result.Skylink] = true
		traffic.UploadSizePeriod += result.Size
		if last24 {
			traffic.UploadSize24Hours += result.Size
		}
	}
	return trafficMap, nil
}

func (db *DB) userDownloadTraffic(ctx context.Context, userID primitive.ObjectID, periodStart time.Time) (map[Referrer]trafficStats, error) {
	c, err := db.staticDownloads.Aggregate(ctx, trafficPipeline(userID, periodStart))
	if err != nil {
		return nil, err
	}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
		}
	}()
	// We need this struct, so we can safely decode both int32 and int64.
	result := struct {
		Size         int64     `bson:"size"`
		Skylink      string    `bson:"skylink"`
		Timestamp    time.Time `bson:"timestamp"`
		Referrer     string    `bson:"referrer"`
		ReferrerType string    `bson:"referrer_type"`
	}{}
	trafficMap := make(map[Referrer]trafficStats)
	var last24 bool
	for c.Next(ctx) {
		if err = c.Decode(&result); err != nil {
			return nil, errors.AddContext(err, "failed to decode DB data")
		}
		ref := Referrer{
			CanonicalName: result.Referrer,
			Type:          result.ReferrerType,
		}
		if _, exists := trafficMap[ref]; !exists {
			trafficMap[ref] = trafficStats{}
		}
		traffic := trafficMap[ref]
		last24 = result.Timestamp.After(time.Now().Add(-1 * time.Hour))
		band := skynet.BandwidthDownloadCost(result.Size)
		traffic.BandwidthPeriod += band
		if last24 {
			traffic.Bandwidth24Hours += band
		}
		traffic.CountTotal++
		if last24 {
			traffic.Count24Hours++
		}
	}
	return trafficMap, nil
}

func (db *DB) userRegistryReadTraffic(ctx context.Context, userID primitive.ObjectID, periodStart time.Time) (map[Referrer]trafficStats, error) {
	filter := bson.D{
		{"user_id", userID},
		{"timestamp", bson.D{{"$gt", periodStart}}},
	}
	c, err := db.staticRegistryReads.Find(ctx, filter)
	//matchStage := bson.D{{"$match", bson.D{
	//	{"user_id", userID},
	//	{"timestamp", bson.D{{"$gt", periodStart}}},
	//}}}
	//c, err := db.staticRegistryReads.Aggregate(ctx, mongo.Pipeline{matchStage})
	//if err != nil {
	//	return nil, err
	//}
	defer func() {
		if errDef := c.Close(ctx); errDef != nil {
			db.staticLogger.Traceln("Error on closing DB cursor.", errDef)
		}
	}()
	// We need this struct, so we can safely decode both int32 and int64.
	result := struct {
		Timestamp    time.Time `bson:"timestamp"`
		Referrer     string    `bson:"referrer"`
		ReferrerType string    `bson:"referrer_type"`
	}{}
	trafficMap := make(map[Referrer]trafficStats)
	var last24 bool
	for c.Next(ctx) {
		if err = c.Decode(&result); err != nil {
			return nil, errors.AddContext(err, "failed to decode DB data")
		}
		ref := Referrer{
			CanonicalName: result.Referrer,
			Type:          result.ReferrerType,
		}
		if _, exists := trafficMap[ref]; !exists {
			trafficMap[ref] = trafficStats{}
		}
		traffic := trafficMap[ref]
		last24 = result.Timestamp.After(time.Now().Add(-1 * time.Hour))
		traffic.BandwidthPeriod += skynet.CostBandwidthRegistryRead
		if last24 {
			traffic.Bandwidth24Hours += skynet.CostBandwidthRegistryRead
		}
		traffic.CountTotal++
		if last24 {
			traffic.Count24Hours++
		}
	}
	return trafficMap, nil
}

// trafficPipeline generates a Mongo aggregation pipeline used for calculating
// the user's upload and download traffic usage.
func trafficPipeline(userID primitive.ObjectID, periodStart time.Time) mongo.Pipeline {
	matchStage := bson.D{{"$match", bson.D{
		{"user_id", userID},
		{"timestamp", bson.D{{"$gt", periodStart}}},
	}}}
	lookupStage := bson.D{
		{"$lookup", bson.D{
			{"from", "skylinks"},
			{"localField", "skylink_id"},
			{"foreignField", "_id"},
			{"as", "skylink_data"},
		}},
	}
	replaceStage := bson.D{
		{"$replaceRoot", bson.D{
			{"newRoot", bson.D{
				{"$mergeObjects", bson.A{
					bson.D{{"$arrayElemAt", bson.A{"$skylink_data", 0}}}, "$$ROOT"},
				},
			}},
		}},
	}
	// These are the fields we don't need.
	projectStage := bson.D{{"$project", bson.D{
		{"_id", 0},
		{"user_id", 0},
		{"skylink_data", 0},
		{"name", 0},
		{"skylink_id", 0},
	}}}
	return mongo.Pipeline{matchStage, lookupStage, replaceStage, projectStage}
}
