// This file contains the logic for interacting with the RarityMon site itself
package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/anaskhan96/soup"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	bolt "go.etcd.io/bbolt"
)

var (
	traitMatcher       = regexp.MustCompile(`([\w\s_-]+):\s([\w\s_-]+)`)
	rankMatcher        = regexp.MustCompile(`Rank\s([0-9]+)\s\/\s([0-9]+)`)
	rarityScoreMatcher = regexp.MustCompile(`Rarity\sScore:\s([0-9\.]+)`)

	ErrorNodeNotFound       = errors.New("could not find the HTML node")
	ErrorNodeLengthMismatch = errors.New("rarity nodes found are unbalanced")
)

const RarityMonURL = "https://www.raritymon.com/Item-details?collection=%s&id=%d"

type Item struct {
	Name   string           `json:"name"`
	Rank   int              `json:"rank"`
	Total  int              `json:"total"`
	Score  float64          `json:"score"`
	Traits map[string]Trait `json:"traits"`
}

type Trait struct {
	Type       string  `json:"type"`
	Name       string  `json:"name"`
	Tier       string  `json:"tier"`
	Percentage float64 `json:"percentage"`
}

func checkNode(node *soup.Root) error {
	if node.Error != nil {
		return node.Error
	} else if node.Pointer == nil {
		return ErrorNodeNotFound
	}
	return nil
}

func parseRank(rank string) (int, int) {
	rank = strings.TrimSpace(rank)

	if rankMatcher.MatchString(rank) {
		groups := rankMatcher.FindAllStringSubmatch(rank, -1)
		ranking, _ := strconv.Atoi(groups[0][1])
		total, _ := strconv.Atoi(groups[0][2])

		return ranking, total
	}

	return -1, -1
}

func parseRarity(rarity string) float64 {
	rarity = strings.TrimSpace(rarity)

	if rarityScoreMatcher.MatchString(rarity) {
		groups := rarityScoreMatcher.FindAllStringSubmatch(rarity, -1)
		rarity, _ := strconv.ParseFloat(groups[0][1], 64)
		return rarity
	}

	return -1
}

func parseTraitEntry(trait string) (string, string) {
	trait = strings.TrimSpace(trait)

	if traitMatcher.MatchString(trait) {
		groups := traitMatcher.FindAllStringSubmatch(trait, -1)
		return groups[0][1], groups[0][2]
	}

	return "", ""
}

func parsePercentage(percentage string) float64 {
	percentage = strings.TrimSpace(strings.ReplaceAll(percentage, "%", ""))
	num, _ := strconv.ParseFloat(percentage, 64)
	return num
}

func FetchItem(collectionId string, id int) (*Item, error) {
	resp, err := soup.Get(fmt.Sprintf(RarityMonURL, collectionId, id))

	if err != nil {
		return nil, err
	}

	rootNode := soup.HTMLParse(resp)

	if err := checkNode(&rootNode); err != nil {
		return nil, err
	}

	itemName := rootNode.Find("h2")

	if err := checkNode(&itemName); err != nil {
		return nil, err
	}

	rarityRank := rootNode.Find("button", "class", "item-rarity-rank")

	if err := checkNode(&rarityRank); err != nil {
		return nil, err
	}
	rarityScore := rootNode.Find("button", "class", "item-trait-data")

	if err := checkNode(&rarityScore); err != nil {
		return nil, err
	}

	traitTitles := rootNode.FindAll("h3", "class", "tier-title")
	traitRarityPercentages := rootNode.FindAll("div", "class", "item-rarity-percentage")
	traitRarityTiers := rootNode.FindAll("div", "class", "item-rarity-tier")

	balanced := len(traitTitles) == len(traitRarityPercentages) && len(traitRarityPercentages) == len(traitRarityTiers)

	if !balanced {
		return nil, ErrorNodeLengthMismatch
	}

	ranking, total := parseRank(rarityRank.Children()[0].NodeValue)
	rarityScoreVal := parseRarity(rarityScore.Children()[0].NodeValue)

	item := &Item{
		Name:   itemName.Children()[0].NodeValue,
		Rank:   ranking,
		Total:  total,
		Score:  rarityScoreVal,
		Traits: make(map[string]Trait),
	}

	for i, traitTitle := range traitTitles {
		traitKey, traitValue := parseTraitEntry(traitTitle.Children()[0].NodeValue)
		traitRarityPercentage := parsePercentage(traitRarityPercentages[i].Children()[0].NodeValue)
		traitRarityTier := traitRarityTiers[i].Children()[0].NodeValue

		item.Traits[traitKey] = Trait{
			Type:       traitKey,
			Name:       traitValue,
			Tier:       traitRarityTier,
			Percentage: traitRarityPercentage,
		}
	}

	return item, nil
}

func GetenvOrDefault(key, def string) string {
	val, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	return val
}

func quickHash(s string) []byte {
	hash := sha256.New()
	hash.Write([]byte(s))
	return hash.Sum(nil)
}

func main() {
	db, err := bolt.Open(GetenvOrDefault("RARITYMON_DB_PATH", "raritymon.db"), 0666, nil)
	if err != nil {
		log.Fatalln(err)
	}
	defer db.Close()

	cacheMiddleware := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			collection := c.Param("collection")
			id := c.Param("id")

			hash := quickHash(collection + ":" + id)

			jsonReturn := []byte{}
			db.View(func(tx *bolt.Tx) error {
				bucket := tx.Bucket([]byte("RarityCache"))
				if bucket != nil {
					cachedJson := bucket.Get(hash)
					if cachedJson != nil {
						jsonReturn = cachedJson
					}
				}
				return nil
			})

			if len(jsonReturn) > 0 {
				return c.JSONBlob(http.StatusOK, jsonReturn)
			}
			return next(c)
		}
	}

	e := echo.New()

	e.Use(middleware.CORS())
	e.GET("/api/:collection/:id", func(c echo.Context) error {
		collection := c.Param("collection")
		id, err := strconv.Atoi(c.Param("id"))

		if err != nil {
			return c.String(http.StatusBadRequest, err.Error())
		}

		item, err := FetchItem(collection, id)

		if err != nil {
			return c.String(http.StatusInternalServerError, err.Error())
		}

		encodedJson, err := json.MarshalIndent(item, " ", "  ")

		if err != nil {
			return c.String(http.StatusInternalServerError, err.Error())
		}

		err = db.Update(func(tx *bolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte("RarityCache"))

			if err != nil {
				return err
			}

			return bucket.Put(quickHash(collection+":"+c.Param("id")), encodedJson)
		})

		if err != nil {
			return c.String(http.StatusInternalServerError, err.Error())
		}

		return c.JSONBlob(http.StatusOK, encodedJson)
	}, cacheMiddleware)
	e.Start(GetenvOrDefault("RARITYMON_WEB_HOST", ":1337"))
}
