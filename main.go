package main

import (
	"context"
	"flag"
	"log"
	"os"
	"time"

	"github.com/paulmach/osm"
	"github.com/paulmach/osm/osmpbf"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/x/bsonx"
)

func main() {
	dbconnection := flag.String("dbconnection", "mongodb://localhost:27017", "Mongo database name")
	dbname := flag.String("dbname", "map", "Mongo database name")
	osmfile := flag.String("osmfile", "", "OSM file")
	initial := flag.Bool("initial", false, "Is initial import")
	concurrency := flag.Int("concurrency", 16, "Concurrency")
	blockSize := flag.Int("block", 1000, "Block size to bulk write")
	flag.Parse()
	ctx, _ := context.WithTimeout(context.Background(), 10*time.Second)
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(*dbconnection))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Disconnect(context.Background())
	db := client.Database(*dbname)
	log.Printf("Started import file %s to db %s", *osmfile, *dbname)
	if *initial {
		log.Println("Initial import")
		createIndexes(db)
		log.Println("Indexes created")
	} else {
		log.Println("Diff import")
	}
	if err := read(db, *osmfile, *initial, *concurrency, *blockSize); err != nil {
		log.Fatal(err)
	}

}

func read(db *mongo.Database, file string, initial bool, concurrency int, blockSize int) error {
	nodes := db.Collection("nodes")
	ways := db.Collection("ways")
	relations := db.Collection("relations")

	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	opts := (new(options.BulkWriteOptions)).SetOrdered(false)
	nc := 0
	wc := 0
	rc := 0

	scanner := osmpbf.New(context.Background(), f, concurrency)
	defer scanner.Close()

	bufferNodes := make([]mongo.WriteModel, 0, blockSize)
	bufferWays := make([]mongo.WriteModel, 0, blockSize)
	bufferRelations := make([]mongo.WriteModel, 0, blockSize)
	for scanner.Scan() {
		o := scanner.Object()
		switch o := o.(type) {
		case *osm.Way:
			nodes := make([]int64, 0, len(o.Nodes))
			for _, v := range o.Nodes {
				nodes = append(nodes, int64(v.ID))
			}

			w := Way{
				OsmID:     int64(o.ID),
				Tags:      convertTags(o.Tags),
				Nodes:     nodes,
				Timestamp: o.Timestamp,
				Version:   o.Version,
				Visible:   o.Visible,
			}
			if initial {
				um := mongo.NewInsertOneModel()
				um.SetDocument(w)
				bufferWays = append(bufferWays, um)
			} else {
				um := mongo.NewUpdateOneModel()
				um.SetUpsert(true)
				um.SetUpdate(w)
				um.SetFilter(bson.M{"osm_id": w.OsmID})
				bufferWays = append(bufferWays, um)
			}
			wc++
		case *osm.Node:
			n := Node{
				OsmID: int64(o.ID),
				Location: Coords{
					Type: "Point",
					Coordinates: []float64{
						o.Lon,
						o.Lat,
					}},
				Tags:      convertTags(o.Tags),
				Version:   o.Version,
				Timestamp: o.Timestamp,
				Visible:   o.Visible,
			}
			if initial {
				um := mongo.NewInsertOneModel()
				um.SetDocument(n)
				bufferNodes = append(bufferNodes, um)
			} else {
				um := mongo.NewUpdateOneModel()
				um.SetUpsert(true)
				um.SetUpdate(n)
				um.SetFilter(bson.M{"osm_id": n.OsmID})
				bufferNodes = append(bufferNodes, um)
			}
			nc++
		case *osm.Relation:
			members := make([]Member, len(o.Members))
			for _, v := range o.Members {
				members = append(members, Member{
					Type:        v.Type,
					Version:     v.Version,
					Orientation: v.Orientation,
					Ref:         v.Ref,
					Role:        v.Role,
					Location: Coords{
						Type: "Point",
						Coordinates: []float64{
							v.Lon,
							v.Lat,
						}},
				})
			}
			r := Relation{
				OsmID:     int64(o.ID),
				Tags:      convertTags(o.Tags),
				Version:   o.Version,
				Timestamp: o.Timestamp,
				Visible:   o.Visible,
				Members:   members,
			}
			if initial {
				um := mongo.NewInsertOneModel()
				um.SetDocument(r)
				bufferRelations = append(bufferRelations, um)
			} else {
				um := mongo.NewUpdateOneModel()
				um.SetUpsert(true)
				um.SetUpdate(r)
				um.SetFilter(bson.M{"osm_id": r.OsmID})
				bufferRelations = append(bufferRelations, um)
			}
			rc++
		}
		if len(bufferNodes) == blockSize {
			if _, err := nodes.BulkWrite(context.Background(), bufferNodes, opts); err != nil {
				return err
			}
			bufferNodes = make([]mongo.WriteModel, 0, blockSize)
			log.Printf("Nodes: %d Ways: %d Relations: %d", nc, wc, rc)
		}
		if len(bufferWays) == blockSize {
			if _, err := nodes.BulkWrite(context.Background(), bufferWays, opts); err != nil {
				return err
			}
			bufferWays = make([]mongo.WriteModel, 0, blockSize)
			log.Printf("Nodes: %d Ways: %d Relations: %d", nc, wc, rc)
		}
		if len(bufferRelations) == blockSize {
			if _, err := nodes.BulkWrite(context.Background(), bufferRelations, opts); err != nil {
				return err
			}
			bufferRelations = make([]mongo.WriteModel, 0, blockSize)
			log.Printf("Nodes: %d Ways: %d Relations: %d", nc, wc, rc)
		}
	}
	if len(bufferNodes) != 0 {
		if _, err := nodes.BulkWrite(context.Background(), bufferNodes, opts); err != nil {
			return err
		}
		log.Printf("Nodes: %d Ways: %d Relations: %d", nc, wc, rc)
	}
	if len(bufferWays) != 0 {
		if _, err := ways.BulkWrite(context.Background(), bufferWays, opts); err != nil {
			return err
		}
		log.Printf("Nodes: %d Ways: %d Relations: %d", nc, wc, rc)
	}
	if len(bufferRelations) != 0 {
		if _, err := relations.BulkWrite(context.Background(), bufferRelations, opts); err != nil {
			return err
		}
		log.Printf("Nodes: %d Ways: %d Relations: %d", nc, wc, rc)
	}
	log.Println("Import done")
	scanErr := scanner.Err()
	if scanErr != nil {
		return scanErr
	}
	return nil
}

func createIndexes(db *mongo.Database) {
	nodes := db.Collection("nodes")
	simpleIndex(nodes, []string{"osm_id"}, true)
	simpleIndex(nodes, []string{"tags"}, false)
	geoIndex(nodes, "location")

	ways := db.Collection("ways")
	simpleIndex(ways, []string{"osm_id"}, true)
	simpleIndex(ways, []string{"tags"}, false)

	relations := db.Collection("relations")
	simpleIndex(relations, []string{"osm_id"}, true)
	simpleIndex(relations, []string{"tags"}, false)
	simpleIndex(relations, []string{"members.ref"}, false)
	geoIndex(relations, "members.coords")
}

func convertTags(tags osm.Tags) map[string]string {
	result := make(map[string]string, len(tags))
	for _, t := range tags {
		result[t.Key] = t.Value
	}
	return result
}

func simpleIndex(col *mongo.Collection, keys []string, unique bool) {
	idxKeys := bsonx.Doc{}
	for _, e := range keys {
		idxKeys.Append(e, bsonx.Int32(1))
	}
	_, _ = col.Indexes().CreateOne(
		context.Background(),
		mongo.IndexModel{
			Keys:    idxKeys,
			Options: options.Index().SetUnique(unique).SetSparse(true).SetBackground(true),
		},
	)
}

func geoIndex(col *mongo.Collection, key string) {
	_, _ = col.Indexes().CreateOne(
		context.Background(),
		mongo.IndexModel{
			Keys: bsonx.Doc{{
				Key: key, Value: bsonx.Int32(1),
			}},
			Options: options.Index().SetSphereVersion(2).SetSparse(true).SetBackground(true),
		},
	)
}
