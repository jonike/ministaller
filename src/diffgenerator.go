package main

import (
  "log"
  "os"
  "path/filepath"
  "sync"
)

type UpdateFileInfo struct {
  Filepath string `json:"path"`
  Sha1 string `json:"sha1"`
  FileSize int64 `json:"size"`
}

type UpdateFilesProvider interface {
  FilesToAdd() []*UpdateFileInfo
  FilesToRemove() []*UpdateFileInfo
  FilesToUpdate() []*UpdateFileInfo
}

type DiffGenerator struct {
  filesToAdd []*UpdateFileInfo
  filesToRemove []*UpdateFileInfo
  filesToUpdate []*UpdateFileInfo
  filesToAddQueue chan *UpdateFileInfo
  filesToRemoveQueue chan *UpdateFileInfo
  filesToUpdateQueue chan *UpdateFileInfo
  errors chan error
  installDirHashes map[string]string
  packageDirHashes map[string]string
  installDirPath string
  packageDirPath string
  keepMissing bool
  forceUpdate bool
}

func (df DiffGenerator) FilesToAdd() []*UpdateFileInfo {
  return df.filesToAdd
}

func (df DiffGenerator) FilesToUpdate() []*UpdateFileInfo {
  return df.filesToUpdate
}

func (df DiffGenerator) FilesToRemove() []*UpdateFileInfo {
  return df.filesToRemove
}

func (df *DiffGenerator) GenerateDiffs() error {
  err := df.calculateHashes()
  if err != nil {
    log.Panic(err)
  }

  var wg sync.WaitGroup

  wg.Add(1)
  go func() {
    for fi := range df.filesToAddQueue {
      df.filesToAdd = append(df.filesToAdd, fi)
    }

    log.Printf("Found %v files to add", len(df.filesToAdd))
    wg.Done()
  }()

  wg.Add(1)
  go func() {
    for fi := range df.filesToRemoveQueue {
      df.filesToRemove = append(df.filesToRemove, fi)
    }

    log.Printf("Found %v files to remove", len(df.filesToRemove))
    wg.Done()
  }()

  wg.Add(1)
  go func() {
    for fi := range df.filesToUpdateQueue {
      df.filesToUpdate = append(df.filesToUpdate, fi)
    }

    log.Printf("Found %v files to update", len(df.filesToUpdate))
    wg.Done()
  }()

  df.generateDirectoryDiff(df.installDirPath, df.packageDirPath)

  wg.Wait()
  log.Println("Differences generated")

  return err
}

func (df *DiffGenerator) calculateHashes() error {
  log.Println("Calculating hashes...")
  var wg sync.WaitGroup

  wg.Add(1)
  go func() {
    df.installDirHashes = CalculateHashes(df.installDirPath)
    wg.Done()
  }()

  wg.Add(1)
  go func() {
    df.packageDirHashes = CalculateHashes(df.packageDirPath)
    wg.Done()
  }()

  wg.Wait()
  log.Println("Hashes calculated")

  return nil
}

func (df *DiffGenerator) generateDirectoryDiff(installDir, packageDir string) {
  log.Printf("Install dir: %v, packageDir: %v", installDir, packageDir);

  go df.findFilesToRemoveOrUpdate(installDir, packageDir)
  go df.findFilesToAdd(installDir, packageDir)
}

func (df *DiffGenerator) findFilesToRemoveOrUpdate(installDir, packageDir string) {
  var wg sync.WaitGroup

  err := filepath.Walk(installDir, func(path string, info os.FileInfo, err error) error {
    if err != nil {
      return err
    }

    if !info.Mode().IsRegular() {
      return nil
    }

    wg.Add(1)

    go func() {
      defer wg.Done()

      relativePath, err := filepath.Rel(df.installDirPath, path)
      if err != nil { log.Panic(err) }
      relativePath = filepath.ToSlash(relativePath)
      packagePath := filepath.Join(df.packageDirPath, relativePath)
      installFileHash := df.installDirHashes[relativePath]

      ufi := &UpdateFileInfo{
        Filepath: relativePath,
        Sha1: installFileHash }

      if pfi, err := os.Stat(packagePath); os.IsNotExist(err) {
        if !df.keepMissing {
          efi, _ := os.Stat(path)
          ufi.FileSize = efi.Size()
          df.filesToRemoveQueue <- ufi
        }
      } else {
        packageFileHash := df.packageDirHashes[relativePath]

        if (packageFileHash != installFileHash) || (df.forceUpdate) {
          ufi.FileSize = pfi.Size()
          df.filesToUpdateQueue <- ufi
        }
      }
    }()

    return nil
  })

  if err != nil {
    log.Printf("Error while update/remove generation: %v", err)
  }

  wg.Wait()
  close(df.filesToRemoveQueue)
  close(df.filesToUpdateQueue)
}

func (df *DiffGenerator) findFilesToAdd(installDir, packageDir string) {
  var wg sync.WaitGroup
  err := filepath.Walk(packageDir, func(path string, info os.FileInfo, err error) error {
    if err != nil {
      return err
    }

    if !info.Mode().IsRegular() {
      return nil
    }

    wg.Add(1)

    go func() {
      defer wg.Done()

      relativePath, err := filepath.Rel(df.packageDirPath, path)
      if err != nil { log.Panic(err) }
      relativePath = filepath.ToSlash(relativePath)
      installPath := filepath.Join(df.installDirPath, relativePath)

      if _, err := os.Stat(installPath); os.IsNotExist(err) {
        packageFileHash := df.packageDirHashes[relativePath]
        efi, _ := os.Stat(path)

        df.filesToAddQueue <- &UpdateFileInfo{
          Filepath: relativePath,
          Sha1: packageFileHash,
          FileSize: efi.Size(),
        }
      }
    }()

    return nil
  })

  if err != nil {
    log.Printf("Error while add generation: %v", err)
  }

  wg.Wait()
  close(df.filesToAddQueue)
}
