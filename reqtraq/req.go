// @llr REQ-0-DDLN-SWL-015
// @llr REQ-0-DDLN-SWL-006
// @llr REQ-0-DDLN-SWL-007
// @llr REQ-0-DDLN-SWL-011
// @llr REQ-0-DDLN-SWL-013

package main

import (
	"bufio"
	"crypto/sha1"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"regexp"
	"sort"
	"strconv"

	"github.com/danieldanciu/gonduit/entities"

	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/daedaleanai/reqtraq/git"
	"github.com/daedaleanai/reqtraq/linepipes"
	"github.com/daedaleanai/reqtraq/lyx"
	"github.com/daedaleanai/reqtraq/phabricator"
)

const projectName = "Reqtraq" //TODO: avoid hardcoding (used in Tasklists and CreateReqGraph)

// RequirementLevel models the four levels of the requirement graph (system, high-level, low-level, code).
type RequirementLevel int

const (
	SYSTEM RequirementLevel = iota
	HIGH
	LOW
	CODE
)

// Maps requirement type (as defined in the Requirement ID) to requirement level
var ReqTypeToReqLevel = map[string]RequirementLevel{
	"SYS": SYSTEM,
	"SWH": HIGH,
	"HWH": HIGH,
	"SWL": LOW,
	"HWL": LOW,
}

type RequirementStatus int

const (
	NOT_STARTED RequirementStatus = iota // req does not have any children, unless code level
	STARTED                              // req does have children but incomplete
	COMPLETED                            // graph complete
)

var reqStatusToString = map[RequirementStatus]string{
	NOT_STARTED: "NOT STARTED",
	STARTED:     "STARTED",
	COMPLETED:   "COMPLETED",
}

func (rs RequirementStatus) String() string { return reqStatusToString[rs] }

var (
	reDiffRev = regexp.MustCompile(`Differential Revision:\s(.*)\s`)
)

// Req represenents a Requirement Node in the graph of Requirements.
// The Attributes map has potential elements;
//  rationale safety_impact verification urgent important mode provenance
type Req struct {
	ID         string // code files do not have an ID, use Path as primary key
	Level      RequirementLevel
	Path       string // .lyx or code file this was found in relative to repo root
	FileHash   string // for code files, the sha1 of the contents
	ParentIds  []string
	Parents    []*Req
	Children   []*Req
	Body       string
	Attributes map[string]string
	Position   int
	Seen       bool
	Status     RequirementStatus
}

func (r *Req) resolveUp() {
	r.Seen = true
	for _, p := range r.Parents {
		p.resolveUp()
	}
}

func (r *Req) resolveDown() RequirementStatus {
	r.Seen = true
	r.Status = COMPLETED
	if r.Level != CODE && len(r.Children) == 0 {
		r.Status = NOT_STARTED
	} else {
		for _, c := range r.Children {
			if c.resolveDown() != COMPLETED {
				r.Status = STARTED
			}
		}
	}
	return r.Status
}

// Title extracts the first line from the requirement's body
func (r *Req) Title() string {
	return strings.TrimSpace(strings.Split(r.Body, "\n")[0])
}

//TODO(dd): this is ugly. Better to separate title from body in the parsing stage
// BodyWithoutTitle removes the title from the body string by
// splitting the body string around the first appearance of
// a newline character and only returning everything that appears
// from the second line onwards
func (r *Req) BodyWithoutTitle() string {
	fields := strings.SplitN(r.Body, "\n", 2)
	if len(fields) != 2 {
		log.Fatal("No valid title in body of requirement with id: ", r.ID)
	}
	return fields[1]
}

// IsDeleted checks if the requirement title starts with 'DELETED'
func (r *Req) IsDeleted() bool {
	return strings.HasPrefix(r.Title(), "DELETED")
}

func (r *Req) CheckAttributes(as []map[string]string) []error {
	var errs []error
	for _, a := range as {
		for k, v := range a {
			switch k {
			case "name":
				if _, ok := r.Attributes[strings.ToUpper(v)]; !ok {
					if !(r.Level == SYSTEM && strings.ToUpper(v) == "PARENTS") {
						errs = append(errs, fmt.Errorf("Requirement '%s' is missing attribute '%s'.\n", r.ID, v))
					}
				}
			case "value":
				aName := strings.ToUpper(a["name"])
				if _, ok := r.Attributes[aName]; ok {
					// attribute exists so needs to be valid
					expr, err := regexp.Compile(v) // TODO(dh) move out so only computed once for each value
					if err != nil {
						log.Fatal(err)
					}
					if !expr.MatchString(r.Attributes[aName]) {
						errs = append(errs, fmt.Errorf("Requirement '%s' has invalid value '%s' in attribute '%s'. Expected %s.\n", r.ID, r.Attributes[aName], aName, v))
					}
				}
			}
		}
	}
	return errs
}

func (r *Req) Tasklists() map[string]*entities.ManiphestTask {
	m := map[string]*entities.ManiphestTask{}
	project, err1 := phabricator.GetProject(projectName)
	if err1 != nil {
		log.Println(err1)
		return m
	}
	// Find and add primary task corresponding to Req
	task, err2 := phabricator.FindTask(r.ID, r.Title(), project.PHID)
	if err2 != nil || task == nil {
		log.Println(err2)
		return m
	}
	m[task.ID] = task
	// Get all tasks that "task" depends on and add them
	for _, phid := range task.DependsOnTaskPHIDs {
		subTask, e := phabricator.FindTaskByPHID(phid)
		if e != nil {
			log.Println(e)
			continue
		}
		m[subTask.ID] = subTask
	}
	return m
}

// @llr REQ-0-DDLN-SWL-009
func (r *Req) Changelists() map[string]string {
	m := map[string]string{}
	if r.Level == LOW {
		var paths []string
		for _, c := range r.Children {
			paths = append(paths, c.Path)
		}
		urls := changelistUrlsForFilepaths(paths)
		for _, url := range urls {
			fields := strings.Split(url, "/")
			m[fields[len(fields)-1]] = url
		}
	}
	return m
}

func changelistUrlsForFilepaths(filepaths []string) []string {
	var urls []string
	for _, path := range filepaths {
		urls = append(urls, changelistUrlsForFilepath(path)...)
	}
	return urls
}

func changelistUrlsForFilepath(filepath string) []string {
	res, err := linepipes.All(linepipes.Run("git", "log", filepath))
	if err != nil {
		log.Fatal(err)
	}

	matches := reDiffRev.FindAllStringSubmatch(res, -1)
	if len(matches) < 1 {
		log.Fatal("Could not find changelist substring for filepath:", filepath)
	}

	var urls []string
	for _, m := range matches {
		if len(m) != 2 {
			log.Fatal("Count not extract changelist substring for filepath: ", filepath)
		}
		urls = append(urls, m[1])
	}

	return urls
}

// @llr REQ-0-DDLN-SWL-015
// A ReqGraph maps IDs and Paths to Req structures.
type reqGraph map[string]*Req

func CreateReqGraph(certdocPath, codePath string) (reqGraph, error) {
	rg := reqGraph{}

	errorResult := ""

	err := filepath.Walk(filepath.Join(git.RepoPath(), certdocPath),
		func(fileName string, info os.FileInfo, err error) error {
			switch strings.ToLower(path.Ext(fileName)) {
			case ".lyx":
				errs := ParseLyx(fileName, rg)
				if len(errs) > 0 {
					errorResult += "Problems found while parsing " + fileName + ":\n"
					for _, v := range errs {
						errorResult += "\t" + v.Error() + "\n"
					}
					errorResult += "\n"
				}
			}
			return nil
		})

	// walk the code
	err = filepath.Walk(filepath.Join(git.RepoPath(), codePath), func(fileName string, info os.FileInfo, err error) error {
		switch strings.ToLower(path.Ext(fileName)) {
		case ".cc", ".c", ".h", ".hh", ".go":
			id := relativePathToRepo(fileName, git.RepoPath())
			if id == "" {
				log.Fatal("Malformed code file path")
			}
			err = parseCode(id, fileName, rg)
			if err != nil {
				errorResult += err.Error()
				errorResult += "\n"
			}
		}
		return nil
	})

	err = rg.Resolve()
	if err != nil {
		errorResult += err.Error()
	}

	if errorResult == "" {
		return rg, nil
	} else {
		return rg, fmt.Errorf(errorResult)
	}
}

// relativePathToRepo returns filePath relative to repoPath by
// removing the path to the repository from filePath
func relativePathToRepo(filePath, repoPath string) string {
	fields := strings.SplitAfterN(filePath, repoPath, 2)
	if len(fields) < 2 {
		return ""
	}
	return fields[1][1:] // omit leading slash
}

func (rg reqGraph) AddReq(req *lyx.Req, path string) error {
	if v := rg[req.ID]; v != nil {
		return fmt.Errorf("Requirement %s in %s already defined in %s", req.ID, path, v.Path)
	}
	level, ok := ReqTypeToReqLevel[req.ReqType()]
	if !ok {
		log.Fatal("Invalid request type ", req.ReqType())
	}

	path = strings.TrimPrefix(path, git.RepoPath())
	rg[req.ID] = &Req{ID: req.ID, Level: level, ParentIds: req.Parents, Path: path, Body: req.Attributes["TEXT"],
		Attributes: req.Attributes, Position: int(req.Position)}
	delete(req.Attributes, "TEXT")
	return nil
}

func (rg reqGraph) CheckAttributes(as []map[string]string) []error {
	var errs []error
	for _, req := range rg {
		if req.Level != CODE {
			errs = append(errs, req.CheckAttributes(as)...)
		}
	}
	return errs
}

// @llr REQ-0-DDLN-SWL-004
func (rg reqGraph) checkReqReferences(certdocPath string) error {

	reParents := regexp.MustCompile(`Parents: REQ-`)

	errorResult := ""

	err := filepath.Walk(filepath.Join(git.RepoPath(), certdocPath),
		func(fileName string, info os.FileInfo, err error) error {

			switch strings.ToLower(path.Ext(fileName)) {
			case ".lyx":
				r, err := os.Open(fileName)
				if err != nil {
					return err
				}

				scan := bufio.NewScanner(r)

				for lno := 1; scan.Scan(); lno++ {
					line := scan.Text()
					// parents have alreay been checked in Resolve(), and we don't throw an eror at the place where the deleted req is defined
					discardRefToDeleted := reParents.MatchString(line) || lyx.ReReqDeleted.MatchString(line)
					parmatch := lyx.ReReqID.FindAllStringSubmatchIndex(line, -1)

					for _, ids := range parmatch {
						reqID := line[ids[0]:ids[1]]
						v, reqFound := rg[reqID]
						if !reqFound {
							errorResult += "Invalid reference to inexistent requirement " + reqID + " in " + fileName + ":" + strconv.Itoa(lno) + "\n"
						} else if v.IsDeleted() && !discardRefToDeleted {
							errorResult += "Invalid reference to deleted requirement " + reqID + " in " + fileName + ":" + strconv.Itoa(lno) + "\n"
						}
					}
				}
			}
			return nil
		})

	if err != nil {
		return err
	}

	if errorResult == "" {
		return nil
	} else {
		return fmt.Errorf(errorResult)
	}
}

func (rg reqGraph) AddCodeRefs(id, fileName, fileHash string, reqIds []string) {
	rg[fileName] = &Req{ID: id, Path: fileName, FileHash: fileHash, ParentIds: reqIds, Level: CODE}
}

// @llr REQ-0-DDLN-SWL-017
func (rg reqGraph) Resolve() error {

	errorResult := ""

	for _, req := range rg {
		if len(req.ParentIds) == 0 && req.Level != SYSTEM {
			errorResult += "Requirement " + req.ID + " in file " + req.Path + " has no parents.\n"
		}
		for _, parentId := range req.ParentIds {
			parent := rg[parentId]
			if parent != nil {
				if parent.IsDeleted() && !req.IsDeleted() {
					if req.Level != CODE {
						errorResult += "Invalid parent of requirement " + req.ID + ": " + parentId + " is deleted.\n"
					} else {
						errorResult += "Invalid reference in file " + req.Path + ": " + parentId + " is deleted.\n"
					}
				}
				parent.Children = append(parent.Children, req)
				req.Parents = append(req.Parents, parent)
			} else {
				if req.Level != CODE {
					errorResult += "Invalid parent of requirement " + req.ID + ": " + parentId + " does not exist.\n"
				} else {
					errorResult += "Invalid reference in file " + req.Path + ": " + parentId + " does not exist.\n"
				}
			}
		}
	}

	if errorResult != "" {
		errorResult += "\n"
		return fmt.Errorf(errorResult)
	}

	for _, req := range rg {
		if req.Level == SYSTEM {
			req.resolveDown()
		}
	}

	for _, req := range rg {
		sort.Sort(byPosition(req.Parents))
		sort.Sort(byPosition(req.Children))
	}

	for _, req := range rg {
		if req.Level == CODE {
			req.resolveUp()
			req.Position = req.Parents[0].Position
		}
	}
	return nil

}

func (rg reqGraph) OrdsByPosition() []*Req {
	var r []*Req
	for _, v := range rg {
		if v.Level == SYSTEM {
			r = append(r, v)
		}
	}
	sort.Sort(byPosition(r))
	return r
}

func (rg reqGraph) CodeFilesByPosition() []*Req {
	var r []*Req
	for _, v := range rg {
		if v.Level == CODE {
			r = append(r, v)
		}
	}
	sort.Sort(byPosition(r))
	return r
}

// Updates the Phabricator tasks associated with each requirement.For each requirement in rg, the method will:
// - find the task associated with the requirement, by searching for the requirement ID in the task title using the Phabricator API
// - if a task was found and the requirement was not deleted, its title and description are updated
// - if a task was found and the requirement was deleted, the task is set as INVALID
// - if the task was not found, it is created and filled in with the following values:
// 	Title: <Req ID> <Req Title>
//	Description: <Requirement Body>
//	Status: Open
//	Tags: Project Abbreviation (e.g. DDLN, VXU, etc.)
//      Parents: the first parent task (Phabricator doesn't yet support multiple parents in the api)
// The method performs a breadth-first search of the requirement graph, which ensures that all parent tasks have already
// been created by the time a child is visited.
func (rg reqGraph) UpdatePhabricatorTasks(filterIDs map[string]bool) error {
	queue := rg.OrdsByPosition()  // breadth-first traversal queue
	enqueued := map[string]bool{} // set of elements that have already been enqueued for traversal
	reqIDToTaskPHID := map[string]string{}
	const projectNameSYS = projectName + "-SYS"
	const projectNameHLR = projectName + "-HLR"
	const projectNameLLR = projectName
	sysProject, err := phabricator.GetOrCreateProject(projectNameSYS, "")
	if err != nil {
		return err
	}

	hlrsProject, err := phabricator.GetOrCreateProject(projectNameHLR, sysProject.PHID)
	if err != nil {
		return err
	}
	llrsProject, err := phabricator.GetOrCreateProject(projectName, hlrsProject.PHID)
	if err != nil {
		return err
	}

	parentTaskTitle := "Implement " + projectName
	parentOfAll, err := phabricator.FindTaskByTitle(parentTaskTitle, sysProject.PHID)
	if err != nil {
		return err
	}
	parentOfAllPHID := ""
	if parentOfAll == nil {
		log.Printf("Creating parent of all requirements: '%s'", parentTaskTitle)
		parentOfAllPHID, err = phabricator.CreateTask(parentTaskTitle, "Meta-task that incorporates all tasks needed to implement "+projectName,
			sysProject.PHID, map[string]string{}, []string{})
		if err != nil {
			return fmt.Errorf("Error creating parent of all tasks, %v", err)
		}
	} else {
		parentOfAllPHID = parentOfAll.PHID
	}
	taskLevelToProjectPHID := map[RequirementLevel]string{SYSTEM: sysProject.PHID, HIGH: hlrsProject.PHID, LOW: llrsProject.PHID}
	for len(queue) > 0 {
		currentReq := queue[0]
		queue = queue[1:]
		if currentReq.Level == CODE {
			continue
		}
		projectPHID := taskLevelToProjectPHID[currentReq.Level]
		task, err := phabricator.FindTask(currentReq.ID, currentReq.Title(), projectPHID)
		if err != nil {
			return fmt.Errorf("Error finding task for requirement %s, caused by\n%v", currentReq.ID, err)
		}

		var parentTaskIDs []string

		if currentReq.Level == SYSTEM {
			parentTaskIDs = []string{parentOfAllPHID}
		} else { // HLR or LLR
			for _, parentReq := range currentReq.Parents {
				taskID, ok := reqIDToTaskPHID[parentReq.ID]
				if !ok {
					return fmt.Errorf("Error updating requirement %s. Parent %s has no corresponding task", currentReq.ID, parentReq.ID)
				}
				parentTaskIDs = append(parentTaskIDs, taskID)
			}
		}
		//TODO: add support for deleted tasks
		if filterIDs[currentReq.ID] { // don't update requirements that are filtered
			if task == nil {
				if !currentReq.IsDeleted() {
					log.Printf("Creating task for requirement %s", currentReq.ID)
					taskPHID, err := phabricator.CreateTask(currentReq.ID+": "+currentReq.Title(), currentReq.BodyWithoutTitle(), projectPHID, currentReq.Attributes, parentTaskIDs)
					if err != nil {
						return fmt.Errorf("Error creating requirement %s, caused by\n%v", currentReq.ID, err)
					}
					reqIDToTaskPHID[currentReq.ID] = taskPHID
				}
			} else {
				if currentReq.IsDeleted() {
					if task.Status != "invalid" {
						log.Printf("Marking task T%s for DELETED requirement %s as invalid", task.ID, currentReq.ID)
						err = phabricator.DeleteTask(task.ID, currentReq.ID+": "+currentReq.Title(), projectPHID)
						if err != nil {
							return fmt.Errorf("Error updating requirement %s, caused by\n%v", currentReq.ID, err)
						}
					}
				} else {
					log.Printf("Updating task T%s for requirement %s", task.ID, currentReq.ID)
					err = phabricator.UpdateTask(task.ID, currentReq.ID+": "+currentReq.Title(), currentReq.BodyWithoutTitle(), projectPHID, currentReq.Attributes, parentTaskIDs)
					if err != nil {
						return fmt.Errorf("Error updating requirement %s, caused by\n%v", currentReq.ID, err)
					}
				}
			}
		}
		if task!=nil {
			reqIDToTaskPHID[currentReq.ID] = task.PHID
		}
		for _, childReq := range currentReq.Children {
			if _, ok := enqueued[childReq.ID]; !ok {
				enqueued[childReq.ID] = true
				queue = append(queue, childReq)
			}
		}
	}
	return nil
}

func (rg reqGraph) DanglingReqsByPosition() []*Req {
	var r []*Req
	for _, reg := range rg {
		if !reg.Seen {
			r = append(r, reg)
		}
	}
	sort.Sort(byPosition(r))
	return r
}

func (rg reqGraph) ReqsWithInvalidRequirementsByPosition() []*Req {
	var r []*Req

	return r
}

type byPosition []*Req

func (a byPosition) Len() int           { return len(a) }
func (a byPosition) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byPosition) Less(i, j int) bool { return a[i].Position < a[j].Position }

var reLLRReference = regexp.MustCompile(`//\s*@llr\s*(REQ-\d+-\w+-SWL-\d+).*`)

func parseCode(id, fileName string, graph reqGraph) error {
	f, err := os.Open(fileName)
	if err != nil {
		return err
	}
	var refs []string
	h := sha1.New()
	// git compatible hash
	if s, err := f.Stat(); err == nil {
		fmt.Fprintf(h, "blob %d", s.Size())
		h.Write([]byte{0})
	}

	scanner := bufio.NewScanner(io.TeeReader(f, h))
	for scanner.Scan() {
		if parts := reLLRReference.FindStringSubmatch(scanner.Text()); len(parts) > 0 {
			refs = append(refs, parts[1])
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if len(refs) > 0 {
		graph.AddCodeRefs(id, fileName, string(h.Sum(nil)), refs)
	}
	return nil
}

func ParseLyx(fileName string, graph reqGraph) []error {

	if err := lyx.IsValidDocName(fileName); err != nil {
		return []error{err}
	}

	reqs, err := lyx.ParseCertdoc(fileName, ioutil.Discard)
	if err != nil {
		return []error{fmt.Errorf("Error parsing %s: %v", fileName, err)}
	}

	isReqPresent := make([]bool, len(reqs))

	var errs []error
	for i, v := range reqs {
		r, err := lyx.ParseReq(v)
		//fmt.Println(i, r, err2)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		errs2 := lintLyxReq(fileName, len(reqs), isReqPresent, r)
		if len(errs2) != 0 {
			errs = append(errs, errs2...)
			continue
		}
		r.Position = uint32(i)
		graph.AddReq(r, fileName)
	}

	return errs
}

type FilterType int

const (
	TitleFilter FilterType = iota
	IdFilter
	BodyFilter
)

type ReqFilter map[FilterType]*regexp.Regexp

// @llr REQ-0-DDLN-SWL-012
// Matches returns true if the requirement matches the filter AND its ID is
// in the diffs map, if any.
func (r *Req) Matches(filter ReqFilter, diffs map[string][]string) bool {
	for t, e := range filter {
		switch t {
		case TitleFilter:
			if !e.MatchString(r.Title()) {
				return false
			}
		case IdFilter:
			if !e.MatchString(r.ID) {
				return false
			}
		case BodyFilter:
			if !e.MatchString(r.Body) {
				return false
			}
		}
	}
	if diffs == nil {
		return true
	}
	_, ok := diffs[r.ID]
	return ok
}