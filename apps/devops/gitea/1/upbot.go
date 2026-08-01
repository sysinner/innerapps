package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/cavaliergopher/grab/v3"
	"github.com/hashicorp/go-version"
)

var (
	pkgIndexUrl     = "https://dl.gitea.com/gitea/"
	pkgInnerListUrl = "https://www.sysinner.cn/in/api/v2/public/package/list?name=gitea&latest_only=true"
	lastVersion     = ""

	// relRevRe matches the trailing package revision suffix ("-r3") on a
	// release version like "1.26.4-r3".
	relRevRe = regexp.MustCompile(`-r\d+$`)
)

func main() {

	{
		b, err := urlCall(pkgInnerListUrl)
		if err != nil {
			panic(err)
		}
		type respData struct {
			Items []struct {
				Release struct {
					Version string `json:"version"`
				} `json:"release"`
			} `json:"items"`
		}
		var rs respData
		if err = json.Unmarshal(b, &rs); err != nil {
			panic(err)
		}
		if len(rs.Items) > 0 {
			// release.version carries the package revision suffix
			// (e.g. "1.26.4-r3"); strip it so go-version parses a clean
			// semver instead of mistaking "-r3" for a prerelease.
			lastVersion = relRevRe.ReplaceAllString(rs.Items[0].Release.Version, "")
		}

		if lastVersion == "" {
			lastVersion = "1.26"
		}
	}

	lastSemver, err := version.NewVersion(lastVersion)
	if err != nil {
		panic(err)
	}

	body, err := urlCall(pkgIndexUrl)
	if err != nil {
		panic(err)
	}

	dom, err := goquery.NewDocumentFromReader(bytes.NewBuffer(body))
	if err != nil {
		panic(err)
	}

	vers := []*version.Version{}

	dom.Find("tr.file").Each(func(i int, s *goquery.Selection) {
		s.Find("span.name").Each(func(i2 int, s2 *goquery.Selection) {
			currSemver, err := version.NewVersion(s2.Text())
			if err != nil {
				return
			}
			if currSemver.LessThanOrEqual(lastSemver) {
				return
			}
			vers = append(vers, currSemver)
		})
	})

	sort.Slice(vers, func(i, j int) bool {
		return vers[i].Compare(vers[j]) < 0
	})
	fmt.Println("versions", vers)

	for i, ver := range vers {
		vs := ver.Segments()
		if i+1 < len(vers) {
			v2s := vers[i+1].Segments()
			if v2s[0] == vs[0] && v2s[1] == vs[1] {
				println("skip", ver.String())
				continue
			}
		}

		if err := download(ver.String()); err != nil {
			return
		}
		for _, v := range [][]string{
			{"linux", "amd64"},
		} {
			pkgname := fmt.Sprintf("gitea_%s_%s_%s.ipk", ver.String(), v[0], v[1])
			if _, err := os.Stat(pkgname); err != nil && os.IsNotExist(err) {
				args := []string{
					"pkg-build",
					"--os", v[0],
					"--arch", v[1],
					"--version", ver.String(),
				}
				fmt.Println("innerstack start : innerstack " + strings.Join(args, " "))
				out, err := exec.Command("innerstack", args...).Output()
				if err != nil {
					fmt.Println("innerstack pkg-build fail", err, string(out))
					continue
				}
				fmt.Println("innerstack pkg-build done : ", pkgname)
			}
			args := []string{
				"pkg-push",
				pkgname,
			}
			fmt.Println("innerstack", args)
			continue
			out, err := exec.Command("innerstack", args...).Output()
			if err != nil {
				fmt.Println("innerstack pkg-push fail", err, string(out))
				continue
			}

			fmt.Println("innerstack pkg-push done : ", pkgname)
		}
	}
}

func download(ver string) error {

	localPath := fmt.Sprintf("deps/gitea-%s-linux-amd64", ver)
	reqUrl := fmt.Sprintf("%s%s/gitea-%s-linux-amd64.xz", pkgIndexUrl, ver, ver)

	fmt.Println("download", localPath, "from", reqUrl)

	_, err := os.Stat(localPath)
	if err == nil {
		return nil
	}

	_, err = os.Stat(localPath + ".xz")
	if err == nil {
		_, err = exec.Command("xz", "-d", localPath+".xz").Output()
		return err
	}

	// create client
	client := grab.NewClient()
	req, _ := grab.NewRequest(localPath+".xz", reqUrl)

	// start download
	fmt.Printf("Downloading %v...\n", req.URL())
	resp := client.Do(req)
	fmt.Printf("  %v\n", resp.HTTPResponse.Status)

	// start UI loop
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

Loop:
	for {
		select {
		case <-t.C:
			fmt.Printf("  transferred %v / %v bytes (%.2f%%)\n",
				resp.BytesComplete(),
				resp.Size,
				100*resp.Progress())

		case <-resp.Done:
			break Loop
		}
	}

	if err := resp.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "Download failed: %v\n", err)
		return err
	}

	fmt.Printf("Download saved to ./%v \n", resp.Filename)

	_, err = exec.Command("xz", "-d", localPath+".xz").Output()

	return err
}

func urlCall(reqUrl string) ([]byte, error) {
	resp, err := http.Get(reqUrl)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return ioutil.ReadAll(resp.Body)
}
