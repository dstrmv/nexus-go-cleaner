package main

import (
	"context"
	"github.com/masahiro331/go-mvn-version"
	slogmulti "github.com/samber/slog-multi"
	"github.com/sonatype-nexus-community/gonexus/rm"
	"github.com/urfave/cli/v3"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

var nexusUrl string
var username string
var password string
var repositoryName string
var keepVersions int64
var dryRun bool
var debug bool
var appVersion = "master"

var file, err = os.OpenFile("./narc.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)

var defaultLogLevel = slog.LevelInfo
var logLevelVar = new(slog.LevelVar)

var logger = slog.New(
	slogmulti.Fanout(
		slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
			Level: logLevelVar,
		}),
		slog.NewTextHandler(file, &slog.HandlerOptions{
			Level: logLevelVar,
		}),
	),
)

type MavenRepositoryItem struct {
	RepositoryItem *nexusrm.RepositoryItem
	MavenVersion   version.Version
}

func (item1 MavenRepositoryItem) compareByVersion(item2 MavenRepositoryItem) int {
	return item1.MavenVersion.Compare(item2.MavenVersion)
}

func main() {

	logLevelVar.Set(defaultLogLevel)

	narc := &cli.Command{
		Name:    "narc",
		Usage:   "Nexus Artifact Retainer/Cleaner (NARC) - keep last N versions in Nexus Maven Repository",
		Version: appVersion,
		Commands: []*cli.Command{

			{
				Name:  "maven",
				Usage: "run maven cleaning",
				Action: func(ctx context.Context, command *cli.Command) error {
					clean()
					return nil
				},
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:        "repository",
						Destination: &repositoryName,
						Usage:       "maven repository name",
						Required:    true,
					},
					&cli.IntFlag{
						Name:        "keep",
						Value:       -1,
						Destination: &keepVersions,
						Usage:       "number of versions to keep. -1 means not delete anything",
						Required:    true,
					},
					&cli.StringFlag{
						Name:        "url",
						Value:       "http://localhost:8081",
						Destination: &nexusUrl,
						Usage:       "Nexus URL",
						Sources:     cli.EnvVars("NEXUS_ROOT_URL"),
						Required:    true,
					},
					&cli.StringFlag{
						Name:        "user",
						Value:       "admin",
						Destination: &username,
						Sources:     cli.EnvVars("NEXUS_USER"),
						Usage:       "Nexus username",
					},
					&cli.StringFlag{
						Name:        "password",
						Value:       "admin",
						Destination: &password,
						Sources:     cli.EnvVars("NEXUS_PASS"),
						Usage:       "Nexus password",
					},
					&cli.BoolFlag{
						Name:        "dry-run",
						Destination: &dryRun,
						Usage:       "Shows artifacts for deletion instead of actually deleting them",
						Value:       false,
					},
					&cli.BoolFlag{
						Name:        "debug",
						Destination: &debug,
						Usage:       "Show debug logs",
						Value:       false,
					},
				},
			},
		},
	}

	err := narc.Run(context.Background(), os.Args)
	if err != nil {
		logger.Error("Failed to run application", "error", err)
	}
}

func clean() {

	if debug {
		logLevelVar.Set(slog.LevelDebug)
	}

	logger.Info("starting cleanup process",
		"repository", repositoryName,
		"keep_versions", keepVersions,
		"dry_run", dryRun)

	startTime := time.Now()
	defer func() {
		logger.Info("cleanup process completed",
			"duration", time.Since(startTime).String())
	}()

	http.DefaultClient.Timeout = 2 * time.Minute
	nexus, nexusErr := nexusrm.New(nexusUrl, username, password)
	if nexusErr != nil {
		logger.Error("failed to create Nexus client", "error", nexusErr)
		os.Exit(1)
	}

	components, componentsErr := nexusrm.GetComponents(nexus, repositoryName)
	if componentsErr != nil {
		logger.Error("failed to get components", "error", componentsErr)
		os.Exit(1)
	}

	logger.Info("retrieved components", "count", len(components))

	mavenItems := repositoryItemsToMavenRepositoryItems(&components)
	groupedByMavenCoordinates := groupMavenRepositoryItemsByMavenCoordinates(mavenItems)

	for key, mavenItem := range groupedByMavenCoordinates {
		logger.Debug("Artifact", key, "contains versions")
		for _, item := range mavenItem {
			logger.Debug(item.MavenVersion.String())
		}
	}

	toDelete, toKeep := versionsToDeleteAndToKeep(groupedByMavenCoordinates)
	logArtifacts(groupedByMavenCoordinates, toDelete, toKeep)

	logger.Info("total artifacts to delete", "count", countVersions(&toDelete))

	totalDeleted := 0
	if !dryRun {
		for _, components := range toDelete {
			totalDeleted += deleteComponents(&nexus, components)
		}
	}

	logger.Info("total deleted", "count", totalDeleted)

}

func logArtifacts(groupedByMavenCoordinates map[string][]MavenRepositoryItem, toDelete map[string][]MavenRepositoryItem, toKeep map[string][]MavenRepositoryItem) {
	for key := range maps.Keys(groupedByMavenCoordinates) {
		for _, item := range toDelete[key] {
			logger.Debug("delete artifact", "artifact", key, "version", item.MavenVersion.String())
		}
		logger.Info("artifact versions count to delete", "artifact", key, "count", len(toDelete[key]))
		for _, item := range toKeep[key] {
			logger.Debug("to keep version", "artifact", key, "count", item.MavenVersion.String())
		}
		logger.Info("artifact versions count to keep", "artifact", key, "count", len(toKeep[key]))
	}
}

func repositoryItemsToMavenRepositoryItems(items *[]nexusrm.RepositoryItem) []MavenRepositoryItem {
	result := make([]MavenRepositoryItem, len(*items))
	for i, item := range *items {
		mavenVersion, errVersion := version.NewVersion(item.Version)
		if errVersion != nil {
			logger.Error("Failed to parse version", errVersion)
		}
		result[i] = MavenRepositoryItem{
			RepositoryItem: &item,
			MavenVersion:   mavenVersion,
		}
	}
	return result
}

func groupMavenRepositoryItemsByMavenCoordinates(items []MavenRepositoryItem) map[string][]MavenRepositoryItem {
	result := make(map[string][]MavenRepositoryItem)
	for _, item := range items {
		mavenCoordinates := strings.Join([]string{item.RepositoryItem.Group, item.RepositoryItem.Name}, ".")
		result[mavenCoordinates] = append(result[mavenCoordinates], item)
	}

	return result
}

func versionsToDeleteAndToKeep(allItems map[string][]MavenRepositoryItem) (itemsToDelete map[string][]MavenRepositoryItem, itemsToKeep map[string][]MavenRepositoryItem) {

	itemsToDelete = make(map[string][]MavenRepositoryItem)
	itemsToKeep = make(map[string][]MavenRepositoryItem)

	for artifactId, components := range allItems {
		slices.SortFunc(components, MavenRepositoryItem.compareByVersion)
		splitIndex := max(0, int64(len(components))-keepVersions)
		itemsToDelete[artifactId] = components[:splitIndex]
		itemsToKeep[artifactId] = components[splitIndex:]
	}

	return itemsToDelete, itemsToKeep
}

func countVersions(items *map[string][]MavenRepositoryItem) int {
	count := 0
	for _, v := range *items {
		count += len(v)
	}
	return count
}

// Returns count of deleted components
func deleteComponents(nexusClient *nexusrm.RM, components []MavenRepositoryItem) int {
	deletedCount := 0
	for _, component := range components {
		err := nexusrm.DeleteComponentByID(*nexusClient, component.RepositoryItem.ID)
		deletedArtifactId := strings.Join([]string{component.RepositoryItem.Group, component.RepositoryItem.Name}, ".")
		deletedArtifactVersion := deletedArtifactId + ":" + component.RepositoryItem.Version
		if err == nil {
			deletedCount++
			logger.Debug("deleted artifact", "artifactId", deletedArtifactVersion)
		} else {
			logger.Error("failed to delete artifact", "artifactId", deletedArtifactId)
		}
	}
	return deletedCount
}
