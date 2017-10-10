/*
 * Minio Client (C) 2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/minio/mc/pkg/console"
	"github.com/minio/mc/pkg/probe"

	// golang does not support flat keys for path matching, find does
	"github.com/minio/minio/pkg/wildcard"
)

// findMessage holds JSON and string values for printing find command output.
type findMessage struct {
	contentMessage
}

// String calls tells the console what to print and how to print it.
func (f findMessage) String() string {
	return console.Colorize("Find", f.contentMessage.Key)
}

// JSON formats output to be JSON output.
func (f findMessage) JSON() string {
	return f.contentMessage.JSON()
}

// nameMatch is similar to filepath.Match but only matches the
// base path of the input, if we couldn't find a match we
// also proceed to look for similar strings alone and print it.
//
// pattern:
// 	{ term }
// term:
// 	'*'         matches any sequence of non-Separator characters
// 	'?'         matches any single non-Separator character
// 	'[' [ '^' ] { character-range } ']'
// 	            character class (must be non-empty)
// 	c           matches character c (c != '*', '?', '\\', '[')
// 	'\\' c      matches character c
// character-range:
// 	c           matches character c (c != '\\', '-', ']')
// 	'\\' c      matches character c
// 	lo '-' hi   matches character c for lo <= c <= hi
//
func nameMatch(pattern, path string) bool {
	matched, e := filepath.Match(pattern, filepath.Base(path))
	errorIf(probe.NewError(e).Trace(pattern, path), "Unable to match with input pattern")
	if !matched {
		for _, pathComponent := range strings.Split(path, "/") {
			matched = pathComponent == pattern
			if matched {
				break
			}
		}
	}
	return matched
}

// pathMatch reports whether path matches the wildcard pattern.
// supports  '*' and '?' wildcards in the pattern string.
// unlike path.Match(), considers a path as a flat name space
// while matching the pattern. The difference is illustrated in
// the example here https://play.golang.org/p/Ega9qgD4Qz .
func pathMatch(pattern, path string) bool {
	return wildcard.Match(pattern, path)
}

// regexMatch reports whether path matches the regex pattern.
func regexMatch(pattern, path string) bool {
	matched, e := regexp.MatchString(pattern, path)
	errorIf(probe.NewError(e).Trace(pattern), "Unable to regex match with input pattern")
	return matched
}

// execFind executes the input command line, additionally formats input
// for the command line in accordance with subsititution arguments.
func execFind(command string) {
	commandArgs := strings.Split(command, " ")

	cmd := exec.Command(commandArgs[0], commandArgs[1:]...)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		console.Fatalln(err)
	}
	console.Println(out.String())
}

// watchFind - enables listening on the input path, listens for all file/object
// created actions. Asynchronously executes the input command line, also allows
// formatting for the command line in accordance with subsititution arguments.
func watchFind(ctx *findContext) {
	// Watch is not enabled, return quickly.
	if !ctx.watch {
		return
	}
	params := watchParams{
		recursive: true,
		events:    []string{"put"},
	}
	watchObj, err := ctx.clnt.Watch(params)
	fatalIf(err.Trace(ctx.targetAlias), "Cannot watch with given params")

	// Enables users to kill using the control + c
	trapCh := signalTrap(os.Interrupt, syscall.SIGTERM)

	// Loop until user CTRL-C the command line.
	for {
		select {
		case <-trapCh:
			console.Println()
			close(watchObj.doneChan)
			return
		case event, ok := <-watchObj.Events():
			if !ok {
				return
			}

			time, e := time.Parse(time.RFC822, event.Time)
			if e != nil {
				errorIf(probe.NewError(e).Trace(event.Time), "Unable to parse event time")
				continue
			}

			find(ctx, contentMessage{
				Key:  getAliasedPath(ctx, event.Path),
				Time: time,
				Size: event.Size,
			})

		case err, ok := <-watchObj.Errors():
			if !ok {
				return
			}
			errorIf(err, "Unable to watch for events.")
			return
		}
	}
}

// Descend at most (a non-negative integer) levels of files
// below the starting-prefix and trims the suffix. This function
// returns path as is without manipulation if the maxDepth is 0
// i.e (not set).
func trimSuffixAtMaxDepth(startPrefix, path, separator string, maxDepth uint) string {
	if maxDepth == 0 {
		return path
	}
	// Remove the requested prefix from consideration, maxDepth is
	// only considered for all other levels excluding the starting prefix.
	path = strings.TrimPrefix(path, startPrefix)
	pathComponents := strings.SplitAfter(path, separator)
	if len(pathComponents) >= int(maxDepth) {
		pathComponents = pathComponents[:maxDepth]
	}
	pathComponents = append([]string{startPrefix}, pathComponents...)
	return strings.Join(pathComponents, "")
}

// Get aliased path used finally in printing, trim paths to ensure
// that we have removed the fully qualified paths and original
// start prefix (targetAlias) is retained. This function also honors
// maxDepth if set then the resultant path will be trimmed at requested
// maxDepth.
func getAliasedPath(ctx *findContext, path string) string {
	separator := string(ctx.clnt.GetURL().Separator)
	prefixPath := ctx.clnt.GetURL().String()
	var aliasedPath string
	if ctx.targetAlias != "" {
		aliasedPath = ctx.targetAlias + strings.TrimPrefix(path, prefixPath)
	} else {
		aliasedPath = path
		// look for prefix path, if found filter at that, Watch calls
		// for example always provide absolute path. So for relative
		// prefixes we need to employ this kind of code.
		if i := strings.Index(path, prefixPath); i > 0 {
			aliasedPath = path[i:]
		}
	}
	return trimSuffixAtMaxDepth(ctx.targetURL, aliasedPath, separator, ctx.maxDepth)
}

func find(ctx *findContext, fileContent contentMessage) {
	// Maxdepth can modify the filepath to end as a directory prefix
	// to be consistent with the find behavior, we wont list directories
	// so any paths which end with a separator are ignored.
	if strings.HasSuffix(fileContent.Key, string(ctx.clnt.GetURL().Separator)) {
		return
	}

	// Match the incoming content, didn't match return.
	if !matchFind(ctx, fileContent) {
		return
	} // For all matching content
	// proceed to either exec, format the output string.
	switch {
	case ctx.execCmd != "":
		execFind(stringsReplace(ctx.execCmd, fileContent))
	case ctx.printFmt != "":
		fileContent.Key = stringsReplace(ctx.printFmt, fileContent)
	}
	printMsg(findMessage{fileContent})
}

// doFind - find is main function body which interprets and executes
// all the input parameters.
func doFind(ctx *findContext) error {
	// If watch is enabled we will wait on the prefix perpetually
	// for all I/O events until cancelled by user, if watch is not enabled
	// following defer is a no-op.
	defer watchFind(ctx)

	// iterate over all content which is within the given directory
	for content := range ctx.clnt.List(true, false, DirNone) {
		if content.Err != nil {
			switch content.Err.ToGoError().(type) {
			// handle this specifically for filesystem related errors.
			case BrokenSymlink:
				errorIf(content.Err.Trace(ctx.clnt.GetURL().String()), "Unable to list broken link.")
				continue
			case TooManyLevelsSymlink:
				errorIf(content.Err.Trace(ctx.clnt.GetURL().String()), "Unable to list too many levels link.")
				continue
			case PathNotFound:
				errorIf(content.Err.Trace(ctx.clnt.GetURL().String()), "Unable to list folder.")
				continue
			case PathInsufficientPermission:
				errorIf(content.Err.Trace(ctx.clnt.GetURL().String()), "Unable to list folder.")
				continue
			case ObjectOnGlacier:
				errorIf(content.Err.Trace(ctx.clnt.GetURL().String()), "")
				continue
			}
			fatalIf(content.Err.Trace(ctx.clnt.GetURL().String()), "Unable to list folder.")
			continue
		}

		// Executes all the find functionalities.
		find(ctx, contentMessage{
			Key:  getAliasedPath(ctx, content.URL.String()),
			Time: content.Time.Local(),
			Size: content.Size,
		})

	}

	// Success, notice watch will execute in defer only if enabled and this call
	// will return after watch is cancelled.
	return nil
}

// stringsReplace - formats the string to remove {} and replace each
// with the appropriate argument
func stringsReplace(args string, fileContent contentMessage) string {
	// replace all instances of {}
	str := args
	if strings.Contains(str, "{}") {
		str = strings.Replace(str, "{}", fileContent.Key, -1)
	}

	// replace all instances of {""}
	if strings.Contains(str, `{""}`) {
		str = strings.Replace(str, `{""}`, strconv.Quote(fileContent.Key), -1)
	}

	// replace all instances of {base}
	if strings.Contains(str, "{base}") {
		str = strings.Replace(str, "{base}", filepath.Base(fileContent.Key), -1)
	}

	// replace all instances of {"base"}
	if strings.Contains(str, `{"base"}`) {
		str = strings.Replace(str, `{"base"}`, strconv.Quote(filepath.Base(fileContent.Key)), -1)
	}

	// replace all instances of {dir}
	if strings.Contains(str, "{dir}") {
		str = strings.Replace(str, "{dir}", filepath.Dir(fileContent.Key), -1)
	}

	// replace all instances of {"dir"}
	if strings.Contains(str, `{"dir"}`) {
		str = strings.Replace(str, `{"dir"}`, strconv.Quote(filepath.Dir(fileContent.Key)), -1)
	}

	// replace all instances of {size}
	if strings.Contains(str, "{size}") {
		str = strings.Replace(str, "{size}", humanize.IBytes(uint64(fileContent.Size)), -1)
	}

	// replace all instances of {"size"}
	if strings.Contains(str, `{"size"}`) {
		str = strings.Replace(str, `{"size"}`, strconv.Quote(humanize.IBytes(uint64(fileContent.Size))), -1)
	}

	// replace all instances of {time}
	if strings.Contains(str, "{time}") {
		str = strings.Replace(str, "{time}", fileContent.Time.Format(printDate), -1)
	}

	// replace all instances of {"time"}
	if strings.Contains(str, `{"time"}`) {
		str = strings.Replace(str, `{"time"}`, strconv.Quote(fileContent.Time.Format(printDate)), -1)
	}

	// replace all instances of {url}
	if strings.Contains(str, "{url}") {
		str = strings.Replace(str, "{url}", getShareURL(fileContent.Key), -1)
	}

	// replace all instances of {"url"}
	if strings.Contains(str, `{"url"}`) {
		str = strings.Replace(str, `{"url"}`, strconv.Quote(getShareURL(fileContent.Key)), -1)
	}

	return str
}

// matchFind matches whether fileContent matches appropriately with standard
// "pattern matching" flags requested by the user, such as "name", "path", "regex" ..etc.
func matchFind(ctx *findContext, fileContent contentMessage) (match bool) {
	match = true
	if match && ctx.ignorePattern != "" {
		match = !pathMatch(ctx.ignorePattern, fileContent.Key)
	}
	if match && ctx.namePattern != "" {
		match = nameMatch(ctx.namePattern, fileContent.Key)
	}
	if match && ctx.pathPattern != "" {
		match = pathMatch(ctx.pathPattern, fileContent.Key)
	}
	if match && ctx.regexPattern != "" {
		match = regexMatch(ctx.regexPattern, fileContent.Key)
	}
	if match && !ctx.olderThan.IsZero() {
		match = fileContent.Time.Before(ctx.olderThan)
	}
	if match && !ctx.newerThan.IsZero() {
		match = fileContent.Time.After(ctx.newerThan) || fileContent.Time.Equal(ctx.newerThan)
	}
	if match && ctx.largerSize > 0 {
		match = int64(ctx.largerSize) < fileContent.Size
	}
	if match && ctx.smallerSize > 0 {
		match = int64(ctx.smallerSize) > fileContent.Size
	}
	return match
}

// parseTime - parses input value into a corresponding time value in
// time.Time by adding the input time duration to local UTC time.Now().
func parseTime(duration string) (time.Time, *probe.Error) {
	if duration == "" {
		return time.Time{}, errInvalidArgument().Trace(duration)
	}

	conversion := map[string]int{
		"d": 1,
		"w": 7,
		"m": 30,
		"y": 365,
	}

	// Parse the incoming pattern if its exact number.
	i, e := strconv.Atoi(duration)
	if e != nil {
		// If cant parse as regular string look for
		// a conversion multiplier, either d,w,m,y.
		p := duration[len(duration)-1:]
		i, e = strconv.Atoi(duration[:len(duration)-1])
		if e != nil {
			// if we still cant parse, user input is invalid, return error.
			return time.Time{}, probe.NewError(e)
		}
		i = i * conversion[strings.ToLower(p)]
	}

	now := UTCNow()

	// Find all time in which the time in which the object was just created is after the current time
	t := time.Date(now.Year(), now.Month(), now.Day()-i, now.Hour(), now.Minute(), 0, 0, time.UTC)

	// if we reach this line, user has passed a valid alphanumeric string
	return t, nil
}

// 7 days in seconds.
var defaultSevenDays = time.Duration(604800) * time.Second

// getShareURL is used in conjunction with the {url} substitution
// argument to generate and return presigned URLs, returns error if any.
func getShareURL(path string) string {
	targetAlias, targetURLFull, _, err := expandAlias(path)
	fatalIf(err.Trace(path), "Unable to expand alias")

	clnt, err := newClientFromAlias(targetAlias, targetURLFull)
	fatalIf(err.Trace(targetAlias, targetURLFull), "Unable to initialize client instance from alias.")

	content, err := clnt.Stat(false)
	fatalIf(err.Trace(targetURLFull, targetAlias), "Unable to lookup file/object.")

	// Skip if its a directory.
	if content.Type.IsDir() {
		return ""
	}

	objectURL := content.URL.String()
	newClnt, err := newClientFromAlias(targetAlias, objectURL)
	fatalIf(err.Trace(targetAlias, objectURL), "Unable to initialize new client from alias.")

	// Set default expiry for each url (point of no longer valid), to be 7 days
	shareURL, err := newClnt.ShareDownload(defaultSevenDays)
	fatalIf(err.Trace(targetAlias, objectURL), "Unable to generate share url.")

	return shareURL
}