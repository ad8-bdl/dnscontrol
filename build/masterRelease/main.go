package main

import (
	"context"
	"log"
	"os"
	"strings"

	"fmt"
	"time"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

const (
	owner = "StackExchange"
	repo  = "dnscontrol"
	tag   = "latest"
)

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

var bg = context.Background

var files = []string{"dnscontrol.exe", "dnscontrol-Linux", "dnscontrol-Darwin"}

func main() {

	tok := os.Getenv("GITHUB_ACCESS_TOKEN")
	if tok == "" {
		log.Fatal("$GITHUB_ACCESS_TOKEN required")
	}
	c := github.NewClient(oauth2.NewClient(bg(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok})))

	for _, f := range files {
		log.Printf("--- %s", f)

		log.Println("Getting release info")
		rel, _, err := c.Repositories.GetReleaseByTag(bg(), owner, repo, tag)
		check(err)

		var found *github.ReleaseAsset
		var foundOld *github.ReleaseAsset
		for _, ass := range rel.Assets {
			if ass.GetName() == f {
				found = &ass
			}
			if ass.GetName()+".old" == f {
				foundOld = &ass
			}
		}
		if foundOld != nil {
			log.Fatalf("%s.old was already found. Previous deploy likely failed. Please check and manually delete.", f)
		}
		if found != nil {
			n := found.GetName() + ".old"
			found.Name = &n
			log.Println("Renaming old asset")
			log.Println(found.GetName(), found.GetID())
			_, _, err = c.Repositories.EditReleaseAsset(bg(), owner, repo, found.GetID(), found)
			check(err)
		}

		log.Println("Uploading new file")
		upOpts := &github.UploadOptions{}
		upOpts.Name = f
		f, err := os.Open(f)
		check(err)
		_, _, err = c.Repositories.UploadReleaseAsset(bg(), owner, repo, rel.GetID(), upOpts, f)
		check(err)

		if found != nil {
			log.Println("Deleting old asset")
			_, err = c.Repositories.DeleteReleaseAsset(bg(), owner, repo, found.GetID())
			check(err)
		}
	}

	log.Println("Editing release body")

	log.Println("Getting release info")
	rel, _, err := c.Repositories.GetReleaseByTag(bg(), owner, repo, tag)
	check(err)

	body := strings.TrimSpace(rel.GetBody())
	lines := strings.Split(body, "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "Last updated:") {
		log.Fatal("Release body is not what I expected. Abort!")
	}
	last = fmt.Sprintf("Last updated: %s", time.Now().Format("Mon Jan 2 2006 @15:04 MST"))
	lines[len(lines)-1] = last
	body = strings.Join(lines, "\n")
	rel.Body = &body
	c.Repositories.EditRelease(bg(), owner, repo, rel.GetID(), rel)

	log.Println("DONE")
}
