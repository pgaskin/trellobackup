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

	"github.com/xlzd/gotp"
)

func main() {
	if len(os.Args) != 2 && len(os.Args) != 3 && len(os.Args) != 4 {
		fmt.Println("Usage: trellobackup (TOKEN_COOKIE | USERNAME PASSWORD [TOTP_SECRET])")
		fmt.Println("Note: If you're using an Atlassian account, you must use the token cookie.")
		os.Exit(1)
	}

	c := &http.Client{}
	c.Jar, _ = cookiejar.New(nil)

	switch len(os.Args) - 1 {
	case 1:
		fmt.Println("Logging in with token cookie")
		u, err := url.Parse("https://trello.com")
		if err != nil {
			panic(err)
		}
		c.Jar.SetCookies(u, []*http.Cookie{&http.Cookie{
			Name:     "token",
			Domain:   "trello.com",
			Path:     "/",
			Expires:  time.Now().Add(time.Hour),
			SameSite: http.SameSiteDefaultMode,
			HttpOnly: false,
			Value:    os.Args[1],
		}})
	case 2, 3:
		fmt.Println("Logging in with Trello account")
		var password, totp string
		username, password, totp := os.Args[1], os.Args[2], ""

		if len(os.Args) == 4 {
			totp = os.Args[3]
		}

		fmt.Println("Getting login token")
		token, err := getLoginToken(c)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: could not get login token: %v\n", err)
			os.Exit(1)
		}

		fmt.Println("Authenticating")
		authentication, err := getAuthentication(c, username, password, "")
		if err != nil && strings.Contains(err.Error(), "TWO_FACTOR_MISSING") {
			if totp == "" {
				fmt.Fprintf(os.Stderr, "Error: could not authenticate: second factor required\n")
				os.Exit(1)
			}
			authentication, err = getAuthentication(c, username, password, totp)
		}
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
	default:
		panic("invalid arguments")
	}

	username, err := getUsername(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not get username: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Logged in as", username)

	fmt.Println("Getting boards")
	boards, err := getBoards(c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: could not get boards: %v\n", err)
		os.Exit(1)
	}

	for _, board := range boards {
		if board.Closed {
			fmt.Printf("Skipping closed board %s (%s) (id: %s)\n", board.Name, board.ShortLink, board.ID)
			continue
		}

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
		return "", fmt.Errorf("could not get login page: %w", err)
	}
	defer resp.Body.Close()

	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("could not read response body: %w", err)
	}

	ms := regexp.MustCompile(`dsc="([a-zA-Z0-9]+)"`).FindStringSubmatch(string(buf))
	if len(ms) != 2 {
		return "", errors.New("could not find dsc (trellobackup may need to be updated)")
	}
	return ms[1], nil
}

func getAuthentication(c *http.Client, username, password, totpSecret string) (string, error) {
	params := url.Values{
		"factors[user]":     []string{username},
		"factors[password]": []string{password},
		"method":            []string{"password"},
	}
	if totpSecret != "" {
		params.Set("factors[totp][password]", gotp.NewDefaultTOTP(totpSecret).Now())
	}

	resp, err := c.PostForm("https://trello.com/1/authentication", params)
	if err != nil {
		return "", fmt.Errorf("could not submit login info (trellobackup may need to be updated): %w", err)
	}
	defer resp.Body.Close()

	var obj struct{ Code, Error string }
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return "", fmt.Errorf("decode json: %w", err)
	} else if obj.Error != "" {
		return "", fmt.Errorf("api error: %s", obj.Error)
	}
	return obj.Code, nil
}

func updateSession(c *http.Client, authentication, token string) error {
	if resp, err := c.PostForm("https://trello.com/1/authorization/session", url.Values{
		"authentication": []string{authentication},
		"dsc":            []string{token},
	}); err != nil {
		return fmt.Errorf("could not send request to api (trellobackup may need to be updated): %w", err)
	} else {
		resp.Body.Close()
	}
	return nil
}

func getUsername(c *http.Client) (string, error) {
	var obj struct{ Username string }

	resp, err := c.Get("https://trello.com/1/members/me?fields=username")
	if err != nil {
		return "", fmt.Errorf("send api request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("response status %s", resp.Status)
	} else if err = json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return "", fmt.Errorf("decode json: %w", err)
	}
	return obj.Username, nil
}

func getBoards(c *http.Client) (boards []struct {
	ShortURL, ShortLink, ID, Name string
	Closed                        bool
}, err error) {
	resp, err := c.Get("https://trello.com/1/Members/me/boards")
	if err != nil {
		return nil, fmt.Errorf("could not send request to api (trellobackup may need to be updated): %w", err)
	}
	defer resp.Body.Close()

	if err := json.NewDecoder(resp.Body).Decode(&boards); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	return boards, nil
}
