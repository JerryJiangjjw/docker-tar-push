package push

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"archive/tar"

	"github.com/docker/distribution"
	"github.com/docker/distribution/manifest/schema2"
	"github.com/opencontainers/go-digest"
	"github.com/silenceper/docker-tar-push/pkg/util"
	"github.com/silenceper/log"
)

type ImagePush struct {
	archivePath      string
	registryEndpoint string
	username         string
	password         string
	skipSSLVerify    bool
	tmpDir           string
	httpClient       *http.Client
}

// NewImagePush new
func NewImagePush(archivePath, registryEndpoint, username, password string, skipSSLVerify bool) *ImagePush {
	registryEndpoint = strings.TrimSuffix(registryEndpoint, "/")
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipSSLVerify},
	}
	return &ImagePush{
		archivePath:      archivePath,
		registryEndpoint: registryEndpoint,
		username:         username,
		password:         password,
		skipSSLVerify:    skipSSLVerify,
		tmpDir:           "/tmp/",
		httpClient:       &http.Client{Transport: tr},
	}
}

// Manifest manifest.json
type Manifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// Push push archive image
func (imagePush *ImagePush) Push() {
	//判断tar包是否正常
	if !util.Exists(imagePush.archivePath) {
		log.Errorf("%s not exists", imagePush.archivePath)
		return
	}

	// 使用系统临时目录
	tmpBase := os.TempDir()
	imagePush.tmpDir = filepath.Join(tmpBase, fmt.Sprintf("docker-tar-push-%d", time.Now().UnixNano()))
	log.Infof("extract archive file %s to %s", imagePush.archivePath, imagePush.tmpDir)

	defer func() {
		err := os.RemoveAll(imagePush.tmpDir)
		if err != nil {
			log.Errorf("remove tmp dir %s error, %v", imagePush.tmpDir, err)
		}
	}()

	// 使用自定义的 tar 解压函数
	err := extractTar(imagePush.archivePath, imagePush.tmpDir)
	if err != nil {
		log.Errorf("unarchive failed, %+v", err)
		return
	}
	data, err := ioutil.ReadFile(imagePush.tmpDir + "/manifest.json")
	if err != nil {
		log.Errorf("read manifest.json failed, %+v", err)
		return
	}

	var manifestObjs []*Manifest
	err = json.Unmarshal(data, &manifestObjs)
	if err != nil {
		log.Errorf("unmarshal manifest.json failed, %+v", err)
		return
	}
	for _, manifestObj := range manifestObjs {
		log.Infof("start push image archive %s", imagePush.archivePath)
		for _, repo := range manifestObj.RepoTags {
			//repo = "test-tar:test-tag"
			image, tag := util.ParseImageAndTag(repo)
			log.Debugf("image=%s,tag=%s", image, tag)

			//push layer
			var layerPaths []string
			for _, layer := range manifestObj.Layers {
				layerPath := filepath.Join(imagePush.tmpDir, layer)
				err = imagePush.pushLayer(layer, image)
				if err != nil {
					log.Errorf("pushLayer %s Failed, %v", layer, err)
					return
				}
				layerPaths = append(layerPaths, layerPath)
			}

			//push image config
			err = imagePush.pushConfig(manifestObj.Config, image)
			if err != nil {
				log.Errorf("push image config failed,%+v", err)
				return
			}

			//push manifest
			log.Infof("start push manifest")
			err = imagePush.pushManifest(layerPaths, manifestObj.Config, image, tag)
			if err != nil {
				log.Errorf("push manifest error,%+v", err)
			}
			log.Infof("push manifest done")
		}
	}
	log.Infof("push image archive %s done", imagePush.archivePath)
}

func (imagePush *ImagePush) checkLayerExist(file, image string) (bool, error) {
	hash, err := util.Sha256Hash(file)
	if err != nil {
		return false, err
	}
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", imagePush.registryEndpoint, image,
		fmt.Sprintf("sha256:%s", hash))
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return false, err
	}
	req.SetBasicAuth(imagePush.username, imagePush.password)
	log.Debugf("HEAD %s", url)
	resp, err := imagePush.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	// 404 means the layer doesn't exist, which is not an error
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("head %s failed, statusCode is %d", url, resp.StatusCode)
	}
	return true, nil
}

func (imagePush *ImagePush) pushManifest(layersPaths []string, imageConfig, image, tag string) error {
	configPath := imagePush.tmpDir + "/" + imageConfig
	obj := &schema2.Manifest{}
	obj.SchemaVersion = schema2.SchemaVersion.SchemaVersion
	obj.MediaType = schema2.MediaTypeManifest
	obj.Config.MediaType = schema2.MediaTypeImageConfig
	configSize, err := util.GetFileSize(configPath)
	if err != nil {
		return err
	}
	obj.Config.Size = configSize
	hash, err := util.Sha256Hash(configPath)
	if err != nil {
		return err
	}
	obj.Config.Digest = digest.Digest("sha256:" + hash)
	for _, layersPath := range layersPaths {
		layerSize, err := util.GetFileSize(layersPath)
		if err != nil {
			return err
		}
		hash, err := util.Sha256Hash(layersPath)
		if err != nil {
			return err
		}
		item := distribution.Descriptor{
			MediaType: schema2.MediaTypeUncompressedLayer,
			Size:      layerSize,
			Digest:    digest.Digest("sha256:" + hash),
		}
		obj.Layers = append(obj.Layers, item)
	}
	data, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", imagePush.registryEndpoint, image, tag)
	req, err := http.NewRequest("PUT", url, bytes.NewBuffer(data))
	if err != nil {
		return err
	}
	req.SetBasicAuth(imagePush.username, imagePush.password)
	log.Debugf("PUT %s", url)
	req.Header.Set("Content-Type", schema2.MediaTypeManifest)
	resp, err := imagePush.httpClient.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("put manifest failed, code is %d", resp.StatusCode)
	}
	return nil
}

func (imagePush *ImagePush) pushConfig(imageConfig, image string) error {
	configPath := filepath.Join(imagePush.tmpDir, imageConfig)
	// check image config exists
	exist, err := imagePush.checkLayerExist(configPath, image)
	if err != nil {
		return fmt.Errorf("check layer exist failed,%+v", err)
	}
	if exist {
		log.Infof("%s Already exist", imageConfig)
		return nil
	}

	log.Infof("start push image config %s", imageConfig)
	url, err := imagePush.startPushing(image)
	if err != nil {
		return fmt.Errorf("startPushing Error, %+v", err)
	}
	return imagePush.chunkUpload(configPath, url)
}

func (imagePush *ImagePush) pushLayer(layer, image string) error {
	layerPath := filepath.Join(imagePush.tmpDir, layer)
	// check layer exists
	exist, err := imagePush.checkLayerExist(layerPath, image)
	if err != nil {
		return fmt.Errorf("check layer exist failed,%+v", err)
	}
	if exist {
		log.Infof("%s Already exist", layer)
		return nil
	}

	url, err := imagePush.startPushing(image)
	if err != nil {
		return fmt.Errorf("startPushing Error, %+v", err)
	}
	return imagePush.chunkUpload(layerPath, url)
}

func (imagePush *ImagePush) chunkUpload(file, url string) error {
	log.Debugf("push file %s to %s", file, url)
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	stat, err := f.Stat() //获取文件状态
	if err != nil {
		return err
	}
	defer f.Close()
	contentSize := stat.Size()
	chunkSize := 2097152
	index, offset := 0, 0
	buf := make([]byte, chunkSize)
	h := sha256.New()
	for {
		n, err := f.Read(buf)
		if err == io.EOF {
			break
		}
		offset = index + n
		index = offset
		log.Infof("Pushing %s ... %.2f%s", file, (float64(offset)/float64(contentSize))*100, "%")

		chunk := buf[0:n]

		h.Write(chunk)

		if int64(offset) == contentSize {
			sum := h.Sum(nil)
			//由于是十六进制表示，因此需要转换
			hash := hex.EncodeToString(sum)
			//last
			req, err := http.NewRequest("PUT",
				fmt.Sprintf("%s&digest=sha256:%s", url, hash), bytes.NewBuffer(chunk))
			if err != nil {
				return err
			}
			req.SetBasicAuth(imagePush.username, imagePush.password)
			log.Debugf("PUT %s", url)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Length", fmt.Sprintf("%d", n))
			req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", index, offset))
			resp, err := imagePush.httpClient.Do(req)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusCreated {
				return fmt.Errorf("PUT chunk layer error,code is %d", resp.StatusCode)
			}
			break
		} else {
			req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(chunk))
			if err != nil {
				return err
			}
			req.SetBasicAuth(imagePush.username, imagePush.password)
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("Content-Length", fmt.Sprintf("%d", n))
			req.Header.Set("Content-Range", fmt.Sprintf("%d-%d", index, offset))
			log.Debugf("PATCH %s", url)
			resp, err := imagePush.httpClient.Do(req)
			if err != nil {
				return err
			}
			location := resp.Header.Get("Location")
			if resp.StatusCode == http.StatusAccepted && location != "" {
				// Handle relative URLs by prepending the registry endpoint if needed
				if strings.HasPrefix(location, "/") {
					location = imagePush.registryEndpoint + location
				}
				url = location
			} else {
				return fmt.Errorf("PATCH chunk file error,code is %d", resp.StatusCode)
			}
		}
	}
	return nil
}

func (imagePush *ImagePush) startPushing(image string) (string, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/uploads/", imagePush.registryEndpoint, image)
	req, err := http.NewRequest("POST", url, nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(imagePush.username, imagePush.password)
	log.Debugf("POST %s", url)
	resp, err := imagePush.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	location := resp.Header.Get("Location")
	log.Debugf("location: %s", location)
	if resp.StatusCode == http.StatusAccepted && location != "" {
		// Handle relative URLs by prepending the registry endpoint if needed
		if strings.HasPrefix(location, "/") {
			location = imagePush.registryEndpoint + location
		}
		log.Debugf("location: %s", location)
		return location, nil
	}
	return "", fmt.Errorf("post %s status is %d", url, resp.StatusCode)
}

// extractTar 自定义的 tar 解压函数，处理符号链接
func extractTar(src, dst string) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()

	tr := tar.NewReader(file)
	// 用于存储符号链接信息
	symlinks := make(map[string]struct {
		linkname string
		header   *tar.Header
	})
	// 用于存储已解压的文件路径
	extractedFiles := make(map[string]string)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// 统一路径分隔符为 forward slash
		header.Name = filepath.ToSlash(header.Name)
		target := filepath.Join(dst, header.Name)

		switch header.Typeflag {
		case tar.TypeSymlink:
			// 统一符号链接目标路径的分隔符
			header.Linkname = filepath.ToSlash(header.Linkname)
			// 记录符号链接信息
			symlinks[target] = struct {
				linkname string
				header   *tar.Header
			}{
				linkname: header.Linkname,
				header:   header,
			}
			log.Debugf("Found symlink: %s -> %s", target, header.Linkname)
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			// 确保目标目录存在
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			// 创建文件
			f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			// 复制文件内容
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
			// 记录已解压的文件
			extractedFiles[header.Name] = target
		}
	}

	// 处理符号链接
	for target, symlink := range symlinks {
		// 尝试在已解压的文件中查找目标文件
		if targetPath, exists := extractedFiles[symlink.linkname]; exists {
			// 确保目标目录存在
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			// 复制文件
			if err := copyFile(targetPath, target); err != nil {
				log.Errorf("Failed to copy file %s to %s: %v", targetPath, target, err)
			} else {
				log.Debugf("Successfully copied %s to %s", targetPath, target)
			}
		} else {
			// 尝试使用相对路径查找
			relPath := filepath.ToSlash(filepath.Join(filepath.Dir(symlink.header.Name), symlink.linkname))
			if targetPath, exists := extractedFiles[relPath]; exists {
				if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
					return err
				}
				if err := copyFile(targetPath, target); err != nil {
					log.Errorf("Failed to copy file %s to %s: %v", targetPath, target, err)
				} else {
					log.Debugf("Successfully copied %s to %s", targetPath, target)
				}
			} else {
				// 尝试使用绝对路径查找
				absPath := filepath.ToSlash(filepath.Join(dst, symlink.linkname))
				if targetPath, exists := extractedFiles[absPath]; exists {
					if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
						return err
					}
					if err := copyFile(targetPath, target); err != nil {
						log.Errorf("Failed to copy file %s to %s: %v", targetPath, target, err)
					} else {
						log.Debugf("Successfully copied %s to %s", targetPath, target)
					}
				} else {
					log.Warnf("Target file %s does not exist for symlink %s", symlink.linkname, target)
				}
			}
		}
	}
	return nil
}

// copyFile 复制文件
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}
