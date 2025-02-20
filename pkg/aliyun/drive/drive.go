// This is a golang package written for https://www.aliyundrive.com/
package drive

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
)

const (
	FolderKind = "folder"
	FileKind   = "file"
	AnyKind    = "any"
)

const (
	apiRefreshToken        = "https://auth.aliyundrive.com/v2/account/token"
	apiList                = "https://api.aliyundrive.com/v2/file/list"
	apiCreateFileWithProof = "https://api.aliyundrive.com/v2/file/create_with_proof"
	apiCompleteUpload      = "https://api.aliyundrive.com/v2/file/complete"
	apiFileGet             = "https://api.aliyundrive.com/v2/file/get"
	apiFileGetByPath       = "https://api.aliyundrive.com/v2/file/get_by_path"
	apiCreateWithFolder    = "https://api.aliyundrive.com/adrive/v2/file/createWithFolders"
	apiTrash               = "https://api.aliyundrive.com/v2/recyclebin/trash"
	apiDelete              = "https://api.aliyundrive.com/v3/file/delete"
	apiBatch               = "https://api.aliyundrive.com/v2/batch"

	fakeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.77 Safari/537.36"
)

const (
	MaxPartSize = 1024 * 1024 * 1024 // 1G
)

var (
	errLivpUpload = errors.New("uploading .livp to album is not supported")
)

type Fs interface {
	Get(ctx context.Context, fullPath string, kind string) (*Node, error)
	List(ctx context.Context, fullPath string) ([]Node, error)
	CreateFolder(ctx context.Context, fullPath string) (*Node, error)
	Rename(ctx context.Context, node *Node, newName string) error
	Move(ctx context.Context, node *Node, dstParent *Node, dstName string) error
	Remove(ctx context.Context, node *Node) error
	Open(ctx context.Context, node *Node, headers map[string]string) (io.ReadCloser, error)
	CreateFile(ctx context.Context, fullPath string, size int64, in io.Reader, overwrite bool) (*Node, error)
	CalcProof(fileSize int64, in *os.File) (*os.File, string, error)
	CreateFileWithProof(ctx context.Context, fullPath string, size int64, in io.Reader, sha1Code string, proofCode string, overwrite bool) (*Node, error)
	Copy(ctx context.Context, node *Node, dstParent *Node, dstName string) error
}

type Config struct {
	RefreshToken string
	IsAlbum      bool
	HttpClient   *http.Client
}

func (config Config) String() string {
	return fmt.Sprintf("Config{RefreshToken: %s}", config.RefreshToken)
}

type Drive struct {
	token
	config     Config
	driveId    string
	rootId     string
	rootNode   Node
	httpClient *http.Client
	mutex      sync.Mutex
}

type token struct {
	accessToken string
	expireAt    int64
}

func (drive *Drive) String() string {
	return fmt.Sprintf("AliyunDrive{driveId: %s}", drive.driveId)
}

func (drive *Drive) request(ctx context.Context, method, url string, headers map[string]string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	req.Header.Set("Referer", "https://www.aliyundrive.com/")
	req.Header.Set("User-Agent", fakeUA)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err2 := drive.httpClient.Do(req)
	if err2 != nil {
		return nil, errors.WithStack(err2)
	}

	return res, nil
}

func (drive *Drive) refreshToken(ctx context.Context) error {
	headers := map[string]string{
		"content-type": "application/json;charset=UTF-8",
	}
	data := map[string]string{
		"refresh_token": drive.config.RefreshToken,
		"grant_type":    "refresh_token",
	}

	var body io.Reader
	b, err := json.Marshal(&data)
	if err != nil {
		return errors.WithStack(err)
	}

	body = bytes.NewBuffer(b)
	res, err := drive.request(ctx, "POST", apiRefreshToken, headers, body)
	if err != nil {
		return errors.WithStack(err)
	}
	defer res.Body.Close()

	var token Token
	b, err = ioutil.ReadAll(res.Body)
	if err != nil {
		return errors.WithStack(err)
	}
	err = json.Unmarshal(b, &token)
	if err != nil {
		return errors.Wrapf(err, `failed to parse response "%s"`, string(b))
	}

	drive.accessToken = token.AccessToken
	drive.expireAt = token.ExpiresIn + time.Now().Unix()
	return nil
}

func (drive *Drive) jsonRequest(ctx context.Context, method, url string, request interface{}, response interface{}) error {
	// Token expired, refresh access
	if drive.expireAt < time.Now().Unix() {
		err := drive.refreshToken(ctx)
		if err != nil {
			return errors.WithStack(err)
		}
	}
	headers := map[string]string{
		"content-type":  "application/json;charset=UTF-8",
		"authorization": "Bearer " + drive.accessToken,
	}

	var bodyBytes []byte
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return errors.WithStack(err)
		}
		bodyBytes = b
	}

	res, err := drive.request(ctx, method, url, headers, bytes.NewBuffer(bodyBytes))
	if err != nil {
		return errors.WithStack(err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		return errors.Wrapf(os.ErrNotExist, `failed to request "%s", got "%d"`, url, res.StatusCode)
	}

	if res.StatusCode >= 400 {
		return errors.Errorf(`failed to request "%s", got "%d"`, url, res.StatusCode)
	}

	if response != nil {
		b, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return errors.WithStack(err)
		}
		err = json.Unmarshal(b, &response)
		if err != nil {
			return errors.Wrapf(err, `failed to parse response "%s"`, string(b))
		}
	}

	return nil
}

func NewFs(ctx context.Context, config *Config) (Fs, error) {
	drive := &Drive{
		config:     *config,
		httpClient: config.HttpClient,
	}

	// get driveId
	driveId := ""
	if config.IsAlbum {
		var albumInfo AlbumInfo
		data := map[string]string{}
		err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/adrive/v1/user/albums_info", &data, &albumInfo)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get driveId")
		}

		driveId = albumInfo.Data.DriveId
	} else {
		var user User
		data := map[string]string{}
		err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/v2/user/get", &data, &user)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get driveId")
		}

		driveId = user.DriveId
	}

	drive.driveId = driveId
	drive.rootId = "root"
	drive.rootNode = Node{
		NodeId: "root",
		Type:   "folder",
		Name:   "root",
	}

	return drive, nil
}

// path must start with "/" and must not end with "/"
func normalizePath(s string) string {
	separator := "/"
	if !strings.HasPrefix(s, separator) {
		s = separator + s
	}

	if len(s) > 1 && strings.HasSuffix(s, separator) {
		s = s[:len(s)-1]
	}
	return s
}

func (drive *Drive) listNodes(ctx context.Context, node *Node) ([]Node, error) {
	data := map[string]interface{}{
		"drive_id":       drive.driveId,
		"parent_file_id": node.NodeId,
		"limit":          200,
		"marker":         "",
	}
	var nodes []Node
	var lNodes *ListNodes
	for {
		if lNodes != nil && lNodes.NextMarker == "" {
			break
		}

		err := drive.jsonRequest(ctx, "POST", apiList, &data, &lNodes)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		nodes = append(nodes, lNodes.Items...)
		data["marker"] = lNodes.NextMarker
	}

	return nodes, nil
}

func (drive *Drive) findNameNode(ctx context.Context, node *Node, name string, kind string) (*Node, error) {
	nodes, err := drive.listNodes(ctx, node)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	for _, d := range nodes {
		if d.Name == name && (kind == AnyKind || d.Type == kind) {
			return &d, nil
		}
	}

	return nil, errors.Wrapf(os.ErrNotExist, `can't find "%s", kind: "%s" under "%s"`, name, kind, node)
}

// https://help.aliyun.com/document_detail/175927.html#pdsgetfilebypathrequest
func (drive *Drive) Get(ctx context.Context, fullPath string, kind string) (*Node, error) {
	fullPath = normalizePath(fullPath)

	if fullPath == "/" || fullPath == "" {
		return &drive.rootNode, nil
	}

	data := map[string]interface{}{
		"drive_id":  drive.driveId,
		"file_path": fullPath,
	}

	var node *Node
	err := drive.jsonRequest(ctx, "POST", apiFileGetByPath, &data, &node)
	// paths with surrounding white spaces (like `/ test / test1 `)
	// can't be found by `get_by_path`
	// https://github.com/K265/aliyundrive-go/issues/3
	if err != nil && !strings.Contains(err.Error(), `got "404"`) {
		return nil, errors.WithStack(err)
	}

	if node != nil && node.Type == kind {
		return node, nil
	}

	parent, name := path.Split(fullPath)
	parentNode, err := drive.Get(ctx, parent, FolderKind)
	if err != nil {
		return nil, errors.Wrapf(err, `failed to request "%s"`, apiFileGetByPath)
	}

	return drive.findNameNode(ctx, parentNode, name, kind)
}

func findNodeError(err error, path string) error {
	return errors.Wrapf(err, `failed to find node of "%s"`, path)
}

func (drive *Drive) List(ctx context.Context, fullPath string) ([]Node, error) {
	fullPath = normalizePath(fullPath)
	node, err := drive.Get(ctx, fullPath, FolderKind)
	if err != nil {
		return nil, findNodeError(err, fullPath)
	}

	nodes, err2 := drive.listNodes(ctx, node)
	if err2 != nil {
		return nil, errors.Wrapf(err2, `failed to list nodes of "%s"`, node)
	}

	return nodes, nil
}

func (drive *Drive) createFolderInternal(ctx context.Context, parent string, name string) (*Node, error) {
	drive.mutex.Lock()
	defer drive.mutex.Unlock()

	node, err := drive.Get(ctx, parent+"/"+name, FolderKind)
	if err == nil {
		return node, nil
	}

	node, err = drive.Get(ctx, parent, FolderKind)
	if err != nil {
		return nil, findNodeError(err, parent)
	}
	body := map[string]string{
		"drive_id":        drive.driveId,
		"check_name_mode": "refuse",
		"name":            name,
		"parent_file_id":  node.NodeId,
		"type":            "folder",
	}
	var createdNode Node
	err = drive.jsonRequest(ctx, "POST", apiCreateFileWithProof, &body, &createdNode)
	if err != nil {
		return nil, errors.Wrap(err, "failed to post create folder request")
	}
	createdNode.Name = name
	return &createdNode, nil
}

func (drive *Drive) CreateFolder(ctx context.Context, fullPath string) (*Node, error) {
	fullPath = normalizePath(fullPath)
	pathLen := len(fullPath)
	i := 0
	var createdNode *Node
	for i < pathLen {
		parent := fullPath[:i]
		remain := fullPath[i+1:]
		j := strings.Index(remain, "/")
		if j < 0 {
			// already at last position
			return drive.createFolderInternal(ctx, parent, remain)
		} else {
			node, err := drive.createFolderInternal(ctx, parent, remain[:j])
			if err != nil {
				return nil, err
			}
			i = i + j + 1
			createdNode = node
		}
	}

	return createdNode, nil
}

func (drive *Drive) checkRoot(node *Node) error {
	if node == nil {
		return errors.New("empty node")
	}
	if node.NodeId == drive.rootId {
		return errors.New("can't operate on root ")
	}
	return nil
}

func (drive *Drive) Rename(ctx context.Context, node *Node, newName string) error {
	if err := drive.checkRoot(node); err != nil {
		return err
	}

	data := map[string]interface{}{
		"check_name_mode": "refuse",
		"drive_id":        drive.driveId,
		"file_id":         node.NodeId,
		"name":            newName,
	}
	err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/v2/file/update", &data, nil)
	if err != nil {
		return errors.Wrap(err, `failed to post rename request`)
	}
	return nil
}

func (drive *Drive) Move(ctx context.Context, node *Node, dstParent *Node, dstName string) error {
	if err := drive.checkRoot(node); err != nil {
		return err
	}

	if dstParent == nil {
		return errors.New("parent node is empty")
	}
	body := map[string]string{
		"drive_id":          drive.driveId,
		"file_id":           node.NodeId,
		"to_parent_file_id": dstParent.NodeId,
		"new_name":          dstName,
	}
	err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/v2/file/move", &body, nil)
	if err != nil {
		return errors.Wrap(err, `failed to post move request`)
	}
	return nil
}

func (drive *Drive) Remove(ctx context.Context, node *Node) error {
	if err := drive.checkRoot(node); err != nil {
		return err
	}

	body := map[string]string{
		"drive_id": drive.driveId,
		"file_id":  node.NodeId,
	}

	err := drive.jsonRequest(ctx, "POST", apiTrash, &body, nil)
	if err != nil {
		return errors.Wrap(err, `failed to post remove request`)
	}
	return nil
}

func (drive *Drive) getDownloadUrl(ctx context.Context, node *Node) (*DownloadUrl, error) {
	var detail DownloadUrl
	data := map[string]string{
		"drive_id": drive.driveId,
		"file_id":  node.NodeId,
	}
	err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/v2/file/get_download_url", &data, &detail)
	if err != nil {
		return nil, errors.Wrapf(err, `failed to get node detail of "%s"`, node)
	}
	return &detail, nil
}

func (drive *Drive) Open(ctx context.Context, node *Node, headers map[string]string) (io.ReadCloser, error) {
	if err := drive.checkRoot(node); err != nil {
		return nil, err
	}

	downloadUrl, err := drive.getDownloadUrl(ctx, node)
	if err != nil {
		return nil, err
	}

	url := downloadUrl.Url
	if url != "" {
		res, err := drive.request(ctx, "GET", url, headers, nil)
		if err != nil {
			return nil, errors.Wrapf(err, `failed to download "%s"`, url)
		}

		return res.Body, nil
	}

	// for iOS live photos (.livp)
	streamsUrl := downloadUrl.StreamsUrl
	if streamsUrl != nil {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for t, u := range streamsUrl {
			name := node.Name + "." + t
			w, err := zw.Create(name)
			if err != nil {
				return nil, errors.Wrapf(err, `failed to creat entry "%s" in zip file`, name)
			}

			res, err := drive.request(ctx, "GET", u, headers, nil)
			if err != nil {
				return nil, errors.Wrapf(err, `failed to download "%s"`, u)
			}

			if _, err := io.Copy(w, res.Body); err != nil {
				return nil, errors.Wrapf(err, `failed to write "%s" to zip`, name)
			}

			_ = res.Body.Close()
		}

		err := zw.Close()
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return io.NopCloser(&buf), nil
	}

	return nil, errors.Errorf(`failed to open "%s"`, node)
}

func CalcSha1(in *os.File) (*os.File, string, error) {
	h := sha1.New()
	_, err := io.Copy(h, in)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to calculate sha1")
	}

	_, _ = in.Seek(0, 0)
	return in, fmt.Sprintf("%X", h.Sum(nil)), nil
}

func calcProof(accessToken string, fileSize int64, in *os.File) (*os.File, string, error) {
	start := CalcProofOffset(accessToken, fileSize)
	sret, _ := in.Seek(start, 0)
	if sret != start {
		_, _ = in.Seek(0, 0)
		return in, "", errors.Errorf("failed to seek file to %d", start)
	}

	buf := make([]byte, 8)
	_, _ = in.Read(buf)
	proofCode := base64.StdEncoding.EncodeToString(buf)
	_, _ = in.Seek(0, 0)
	return in, proofCode, nil
}

func (drive *Drive) CalcProof(fileSize int64, in *os.File) (*os.File, string, error) {
	return calcProof(drive.accessToken, fileSize, in)
}

func (drive *Drive) CreateFile(ctx context.Context, fullPath string, size int64, in io.Reader, overwrite bool) (*Node, error) {
	sha1Code := ""
	proofCode := ""

	fin, ok := in.(*os.File)
	if ok {
		in, sha1Code, _ = CalcSha1(fin)
		in, proofCode, _ = drive.CalcProof(size, fin)
	}

	return drive.CreateFileWithProof(ctx, fullPath, size, in, sha1Code, proofCode, overwrite)
}

func makePartInfoList(size int64) []*PartInfo {
	partInfoNum := 0
	if size%MaxPartSize > 0 {
		partInfoNum++
	}
	partInfoNum += int(size / MaxPartSize)
	list := make([]*PartInfo, partInfoNum)
	for i := 0; i < partInfoNum; i++ {
		list[i] = &PartInfo{
			PartNumber: i + 1,
		}
	}
	return list
}

func (drive *Drive) CreateFileWithProof(ctx context.Context, fullPath string, size int64, in io.Reader, sha1Code string, proofCode string, overwrite bool) (*Node, error) {
	fullPath = normalizePath(fullPath)
	if strings.HasSuffix(strings.ToLower(fullPath), ".livp") {
		return nil, errLivpUpload
	}

	if overwrite {
		node, err := drive.Get(ctx, fullPath, FileKind)
		if err == nil {
			err = drive.Remove(ctx, node)
			if err != nil {
				return nil, errors.Wrapf(err, `failed to overwrite "%s", can't remove file`, fullPath)
			}
		}
	}

	i := strings.LastIndex(fullPath, "/")
	parent := fullPath[:i]
	name := fullPath[i+1:]
	_, err := drive.CreateFolder(ctx, parent)
	if err != nil {
		return nil, errors.Wrapf(err, `failed to create folder "%s"`, parent)
	}

	node, err := drive.Get(ctx, parent, FolderKind)
	if err != nil {
		return nil, findNodeError(err, parent)
	}

	var proofResult ProofResult

	proof := &FileProof{
		DriveID:         drive.driveId,
		PartInfoList:    makePartInfoList(size),
		ParentFileID:    node.NodeId,
		Name:            name,
		Type:            "file",
		CheckNameMode:   "auto_rename",
		Size:            size,
		ContentHash:     sha1Code,
		ContentHashName: "sha1",
		ProofCode:       proofCode,
		ProofVersion:    "v1",
	}

	{
		err = drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/v2/file/create_with_proof", proof, &proofResult)
		if err != nil {
			return nil, errors.Wrap(err, `failed to post create file request`)
		}

		if proofResult.RapidUpload {
			// rapid upload
			return drive.Get(ctx, fullPath, FileKind)
		}

		if len(proofResult.PartInfoList) < 1 {
			return nil, errors.New(`failed to extract uploadUrl`)
		}
	}

	for _, part := range proofResult.PartInfoList {
		partReader := io.LimitReader(in, MaxPartSize)
		req, err := http.NewRequestWithContext(ctx, "PUT", part.UploadUrl, partReader)
		if err != nil {
			return nil, errors.Wrap(err, "failed to create upload request")
		}
		resp, err := drive.httpClient.Do(req)
		if err != nil {
			return nil, errors.Wrap(err, "failed to upload file")
		}
		resp.Body.Close()
	}

	var createdNode Node
	{
		body := map[string]interface{}{
			"drive_id":  drive.driveId,
			"file_id":   proofResult.FileId,
			"upload_id": proofResult.UploadId,
		}

		err := drive.jsonRequest(ctx, "POST", apiCompleteUpload, &body, &createdNode)
		if err != nil {
			return nil, errors.Wrap(err, `failed to post upload complete request`)
		}
	}
	return &createdNode, nil
}

// https://help.aliyun.com/document_detail/175927.html#pdscopyfilerequest
func (drive *Drive) Copy(ctx context.Context, node *Node, dstParent *Node, dstName string) error {
	if dstParent == nil {
		return errors.New("parent node is empty")
	}
	body := map[string]string{
		"drive_id":          drive.driveId,
		"file_id":           node.NodeId,
		"to_parent_file_id": dstParent.NodeId,
		"new_name":          dstName,
	}
	err := drive.jsonRequest(ctx, "POST", "https://api.aliyundrive.com/v2/file/copy", &body, nil)
	if err != nil {
		return errors.Wrap(err, `failed to post copy request`)
	}

	return nil
}
