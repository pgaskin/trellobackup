package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Println("Usage: trellobackup USERNAME PASSWORD")
		os.Exit(1)
	}
	username, password := os.Args[1], os.Args[2]

	c := &http.Client{}
	c.Jar, _ = cookiejar.New(nil)

	fmt.Println("Getting login token")
	token, err := getLoginToken(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not get login token: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Authenticating")
	authentication, err := getAuthentication(c, username, password)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not authenticate: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Updating session info")
	err = updateSession(c, authentication, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not update session info: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Getting boards")
	boards, err := getBoards(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not get boards: %v\n", err)
		os.Exit(1)
	}

	for _, board := range boards {
		fmt.Printf("Backing up %s (%s) (id: %s)\n", board.Name, board.ShortLink, board.ID)
		fmt.Println("--> Saving JSON")
		resp, err := c.Get(board.ShortURL + ".json")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not get board JSON (trellobackup may need to be updated): %v\n", err)
			os.Exit(1)
		}

		buf, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not read response body: %v\n", err)
			os.Exit(1)
		}

		err = ioutil.WriteFile(fmt.Sprintf(
			"trello_%s_%s_%s_%s.json",
			time.Now().Format("2006-01-02_15-04"),
			username,
			board.ID,
			regexp.MustCompile("[^a-zA-Z0-9_)(-]+").ReplaceAllString(board.Name, ""),
		), buf, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not save file: %v\n", err)
			os.Exit(1)
		}

		for _, t := range []string{"attachments", "backgrounds"} {
			ts := strings.TrimRight(t, "s")

			fmt.Printf("--> Downloading %s\n", t)
			for _, m := range regexp.MustCompile(`"url": ?"(https?://trello-`+t+`.s3.amazonaws.com/[^"]+)"`).FindAllStringSubmatch(string(buf), -1) {
				fmt.Printf("    Downloading %s %s\n", ts, m[1])

				u, err := url.Parse(m[1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: could not parse %s url: %v\n", ts, err)
					os.Exit(1)
				}

				fn := filepath.Join(t, strings.Replace(u.Path, "/", "_", -1))
				if _, err := os.Stat(fn); err == nil {
					continue // already downloaded
				}

				resp, err := c.Get(m[1])
				if err != nil {
					fmt.Fprintf(os.Stderr, "Error: could not get %s: %v\n", ts, err)
					os.Exit(1)
				}

				os.MkdirAll(t, 0755)
				f, err := os.Create(fn)
				if err != nil {
					resp.Body.Close()
					fmt.Fprintf(os.Stderr, "Error: could not create file for %s: %v\n", ts, err)
					os.Exit(1)
				}

				_, err = io.Copy(f, resp.Body)
				resp.Body.Close()
				if err != nil {
					resp.Body.Close()
					fmt.Fprintf(os.Stderr, "Error: could not download %s: %v\n", ts, err)
					os.Exit(1)
				}
			}
		}
	}

	fmt.Println("Successfully backed up Trello data")
	os.Exit(0)
}

func getLoginToken(c *http.Client) (string, error) {
	resp, err := c.Get("https://trello.com/login")
	if err != nil {
		return "", wrap("could not get login page", err)
	}
	defer resp.Body.Close()

	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", wrap("could not read response body", err)
	}

	ms := regexp.MustCompile(`dsc="([a-zA-Z0-9]+)"`).FindStringSubmatch(string(buf))
	if len(ms) != 2 {
		return "", errors.New("could not find dsc (trellobackup may need to be updated)")
	}

	return ms[1], nil
}

func getAuthentication(c *http.Client, username, password string) (string, error) {
	resp, err := c.PostForm("https://trello.com/1/authentication", url.Values{
		"factors[user]":     []string{username},
		"factors[password]": []string{password},
		"method":            []string{"password"},
	})
	if err != nil {
		return "", wrap("could not submit login info (trellobackup may need to be updated)", err)
	}
	defer resp.Body.Close()

	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", wrap("could not read response body", err)
	}

	var obj struct{ Code, Error string }
	err = json.Unmarshal(buf, &obj)
	if err == nil && obj.Error != "" {
		err = errors.New(obj.Error)
	}
	if err != nil {
		return "", wrap("api error", err)
	}

	return obj.Code, nil
}

func updateSession(c *http.Client, authentication, token string) error {
	resp, err := c.PostForm("https://trello.com/1/authorization/session", url.Values{
		"authentication": []string{authentication},
		"dsc":            []string{token},
	})
	if err != nil {
		return wrap("could not send request to api (trellobackup may need to be updated)", err)
	}
	defer resp.Body.Close()

	ioutil.ReadAll(resp.Body)

	return nil
}

func getBoards(c *http.Client) ([]struct{ ShortURL, ShortLink, ID, Name string }, error) {
	resp, err := c.Get("https://trello.com/1/Members/me/boards")
	if err != nil {
		return nil, wrap("could not send request to api (trellobackup may need to be updated)", err)
	}

	buf, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, wrap("could not read response body", err)
	}

	var boards []struct{ ShortURL, ShortLink, ID, Name string }
	err = json.Unmarshal(buf, &boards)
	if err != nil {
		return nil, wrap("could not parse response body", err)
	}

	return boards, nil
}

func wrap(msg string, err error) error {
	return fmt.Errorf("%s: %v", msg, err)
}
