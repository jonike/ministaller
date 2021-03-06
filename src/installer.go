package main

import (
  "path"
  "os"
  "sync"
  "sort"
  "io"
  "io/ioutil"
  "path/filepath"
  "os/exec"
  "log"
)

const (
  CopyPrice = 100
  RenamePrice = CopyPrice

  RemoveBackupPrice = 30
  RemoveFactor = RenamePrice
  UpdateFactor = RenamePrice + CopyPrice
  AddFactor = CopyPrice
)

const (
  BackupExt = ".bak"
)

type BackupPair struct {
  relpath string
  newpath string
}

type CopyRequest struct {
  from, to string
}

type ProgressHandler interface {
  HandleSystemMessage(message string)
  HandlePercentChange(percent int)
  HandleFinish()
}

type LogProgressHandler struct {
}

type ProgressReporter struct {
  grandTotal uint64
  currentProgress uint64
  progressChan chan int64  
  progressWG sync.WaitGroup
  percent int //0..100
  systemMessageChan chan string
  finished chan bool
  progressHandler ProgressHandler
}

type PackageInstaller struct {
  backups map[string]string
  backupsChan chan BackupPair
  backupsWG sync.WaitGroup
  progressReporter *ProgressReporter
  installDir string
  packageDir string
  removeSelfPath string // if updating the installer
  failInTheEnd bool // for debugging purposes
}

func (pi *PackageInstaller) Install(filesProvider UpdateFilesProvider) error {
  defer func() {
    if r := recover(); r != nil {
      log.Printf("Recovered in install... %v", r)
      pi.afterFailure(filesProvider)
    }
  }()
  
  pi.progressReporter.grandTotal = pi.calculateGrandTotals(filesProvider)
  
  go pi.progressReporter.reportingLoop()

  pi.beforeInstall()

  err := pi.installPackage(filesProvider)

  if (err == nil) && (!pi.failInTheEnd) {
    pi.afterSuccess()
  } else {
    pi.afterFailure(filesProvider)
  }
  
  pi.teardown()

  return err
}

func (pi *PackageInstaller) calculateGrandTotals(filesProvider UpdateFilesProvider) uint64 {
  var sum uint64

  for _, fi := range filesProvider.FilesToRemove() {
    sum += uint64(fi.FileSize * RemoveFactor) / 100
    sum += uint64(fi.FileSize * RemoveBackupPrice) / 100
  }

  for _, fi := range filesProvider.FilesToUpdate() {
    sum += uint64(fi.FileSize * UpdateFactor) / 100
    sum += uint64(fi.FileSize * RemoveBackupPrice) / 100
  }

  for _, fi := range filesProvider.FilesToAdd() {
    sum += uint64(fi.FileSize * AddFactor) / 100
  }

  return sum
}

func (pi *PackageInstaller) beforeInstall() {
  log.Println("Before install")
  pi.removeOldBackups()
}

func (pi *PackageInstaller) installPackage(filesProvider UpdateFilesProvider) (err error) {
  log.Println("Installing package...")

  go pi.accountBackups()  
  
  defer func() {
    close(pi.backupsChan)
  }()

  pi.progressReporter.sendSystemMessage("Removing components...")
  err = pi.removeFiles(filesProvider.FilesToRemove())
  if err != nil {
    return err
  }

  pi.progressReporter.sendSystemMessage("Updating components...")
  err = pi.updateFiles(filesProvider.FilesToUpdate())
  if err != nil {
    return err
  }

  pi.progressReporter.sendSystemMessage("Adding components...")
  err = pi.addFiles(filesProvider.FilesToAdd())
  if err != nil {
    return err
  }

  log.Println("Waiting for backups to finish accounting...")
  pi.backupsWG.Wait()

  return nil
}

func (pi *PackageInstaller) accountBackups() {
  for bp := range pi.backupsChan {
    pi.backups[bp.relpath] = bp.newpath
    pi.backupsWG.Done()
  }
  
  log.Printf("Backups accounting finished. %v backups available", len(pi.backups))
}

func (pi *PackageInstaller) afterSuccess() {
  log.Println("After success")
  pi.progressReporter.sendSystemMessage("Finishing the installation...")
  pi.removeBackups()
  cleanupEmptyDirs(pi.installDir)
}

func (pi *PackageInstaller) afterFailure(filesProvider UpdateFilesProvider) {
  log.Println("After failure")
  pi.progressReporter.sendSystemMessage("Cleaning up...")
  purgeFiles(pi.installDir, filesProvider.FilesToAdd())
  pi.restoreBackups()
  pi.removeBackups()
  cleanupEmptyDirs(pi.installDir)
}

func (pi *PackageInstaller) teardown() {
  log.Println("Teardown stage!")
  
  pi.progressReporter.waitProgressReported()
  pi.progressReporter.shutdown()
  pi.progressReporter.receiveFinish()
}

func copyFile(src, dst string) (err error) {
  log.Printf("About to copy file %v to %v", src, dst)

  fi, err := os.Stat(src)
  if err != nil { return err }
  sourceMode := fi.Mode()

  in, err := os.Open(src)
  if err != nil {
    log.Printf("Failed to open source: %v", err)
    return err
  }

  defer in.Close()

  out, err := os.OpenFile(dst, os.O_RDWR | os.O_TRUNC | os.O_CREATE, sourceMode)
  if err != nil {
    log.Printf("Failed to create destination: %v", err)
    return
  }

  defer func() {
    cerr := out.Close()
    if err == nil {
      err = cerr
    }
  }()

  if _, err = io.Copy(out, in); err != nil {
    return
  }

  err = out.Sync()
  return
}

func (pi *PackageInstaller) backupFile(relpath string) error {
  log.Printf("Backing up %v", relpath)

  oldpath := path.Join(pi.installDir, relpath)
  backupPath := relpath + BackupExt

  newpath := path.Join(pi.installDir, backupPath)
  // remove previous backup if any
  os.Remove(newpath)

  // assume backups are ALWAYS created in the same directory
  // otherwise os.Rename() could be screwed with different harddrives
  err := os.Rename(oldpath, newpath)

  if err == nil {
    pi.backupsWG.Add(1)
    go func() {
      pi.backupsChan <- BackupPair{relpath: relpath, newpath: newpath}
    }()
  } else {
    log.Printf("Backup failed: %v", err)
  }

  return err
}

func (pi *PackageInstaller) restoreBackups() {
  log.Printf("Restoring %v backups", len(pi.backups))
  var wg sync.WaitGroup

  for relpath, backuppath := range pi.backups {
    wg.Add(1)

    go func(relativePath, pathToRestore string) {
      defer wg.Done()

      oldpath := path.Join(pi.installDir, relativePath)
      log.Printf("Restoring %v to %v", pathToRestore, oldpath)

      // backups are supposed to be in the same location as files
      // so rename operaion will not be screwed with by paths
      // on different harddrives
      err := os.Rename(pathToRestore, oldpath)

      if err != nil {
        log.Printf("Error while restoring %v: %v", pathToRestore, err)
      }
    }(relpath, backuppath)
  }

  wg.Wait()
}

func (pi *PackageInstaller) removeOldBackups() {
  backeduppath := currentExeFullPath + BackupExt
  err := os.Remove(backeduppath)
  if err == nil {
    log.Println("Old installer backup removed", backeduppath)
  } else if os.IsNotExist(err) {
    log.Println("Old installer backup was not found")
  } else {
    log.Printf("Error while removing old backup: %v", err)
  }
}

func (pi *PackageInstaller) removeBackups() {
  log.Printf("Removing %v backups", len(pi.backups))

  selfpath, err := filepath.Rel(pi.installDir, currentExeFullPath)
  if err == nil {
    if backuppath, ok := pi.backups[selfpath]; ok {
      pi.removeSelfPath = backuppath
      delete(pi.backups, selfpath)
      log.Printf("Removed exe path %v from backups. %v backups left", selfpath, len(pi.backups))
    }
  }

  for _, backuppath := range pi.backups {
    log.Printf("Removing %v", backuppath)
    err := os.Remove(backuppath)
    if err != nil {
      log.Printf("Error while removing %v: %v", backuppath, err)
    }

    pi.progressReporter.accountBackupRemove()
  }

  log.Println("Backups removed")
}

func (pi *PackageInstaller) removeFiles(files []*UpdateFileInfo) error {
  log.Printf("Removing %v files", len(files))

  for _, fi := range files {
    pathToRemove, filesize := fi.Filepath, fi.FileSize

    fullpath := filepath.Join(pi.installDir, pathToRemove)
    log.Printf("Removing file %v", fullpath)

    // real removal will happen in the end when backup will be removed
    err := pi.backupFile(pathToRemove)

    if err != nil {
      log.Printf("Removing file %v failed: %v", pathToRemove, err)      
    }

    pi.progressReporter.accountRemove(filesize)
  }

  return nil
}

func (pi *PackageInstaller) updateFiles(files []*UpdateFileInfo) error {
  log.Printf("Updating %v files", len(files))
  var err error

  for _, fi := range files {
    pathToUpdate, filesize := fi.Filepath, fi.FileSize

    oldpath := path.Join(pi.installDir, pathToUpdate)
    log.Printf("Updating file %v", oldpath)

    err = pi.backupFile(pathToUpdate)
    if err != nil { log.Printf("Error while backing up %v: %v", pathToUpdate, err) }

    newpath := path.Join(pi.packageDir, pathToUpdate)
    err = os.Remove(oldpath)
    if err != nil { log.Printf("Error while removing %v: %v", oldpath, err) }

    // just os.Rename does not work if files are on different drive
    err = copyFile(newpath, oldpath)
    pi.progressReporter.accountUpdate(filesize)
    
    if err != nil {
      log.Printf("Updating file %v failed: %v", pathToUpdate, err)
      break
    }
  }

  return err
}

func (pi *PackageInstaller) addFiles(files []*UpdateFileInfo) error {
  log.Printf("Adding %v files", len(files))

  for _, fi := range files {
    pathToAdd, filesize := fi.Filepath, fi.FileSize

    oldpath := path.Join(pi.installDir, pathToAdd)
    ensureDirExists(oldpath)

    log.Printf("Adding file %v", pathToAdd)

    newpath := path.Join(pi.packageDir, pathToAdd)
    err := copyFile(newpath, oldpath)

    if err != nil {
      log.Printf("Adding file %v failed: %v", pathToAdd, err)
      return err
    } else {
      pi.progressReporter.accountAdd(filesize)
    }
  }
  
  return nil
}

func (pi *PackageInstaller) removeSelfIfNeeded() {
  if len(pi.removeSelfPath) == 0 {
    log.Println("No need to remove itself")
    return
  }

  // TODO: move this to windows-only define
  pathToRemove := filepath.FromSlash(pi.removeSelfPath)
  log.Println("Removing exe backup", pathToRemove)
  cmd := exec.Command("cmd", "/C", "ping localhost -n 2 -w 5000 > nul & del", pathToRemove)
  err := cmd.Start()
  if err != nil {
    log.Println(err)
  }
}

func purgeFiles(root string, files []*UpdateFileInfo) {
  log.Printf("Purging %v files", len(files))

  for _, fi := range files {
    fullpath := path.Join(root, fi.Filepath)
    log.Printf("Purging file %v", fullpath)
    err := os.Remove(fullpath)
    if err != nil {
      log.Printf("Error while purging %v: %v", fullpath, err)
    }
  }

  log.Println("Finished purging files")
}

func ensureDirExists(fullpath string) (err error) {
  log.Printf("Ensuring directory exists for %v", fullpath)
  dirpath := path.Dir(fullpath)
  err = os.MkdirAll(dirpath, os.ModeDir)
  if err != nil {
    log.Printf("Failed to create directory %v", dirpath)
  }

  return err
}

type ByLength []string

func (s ByLength) Len() int {
    return len(s)
}
func (s ByLength) Swap(i, j int) {
    s[i], s[j] = s[j], s[i]
}
func (s ByLength) Less(i, j int) bool {
    return len(s[i]) > len(s[j])
}

func cleanupEmptyDirs(root string) {
  dirs := make([]string, 0, 10)

  err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
    if err != nil {
      return err
    }

    if info.Mode().IsDir() {  
      dirs = append(dirs, path)
    }
    
    return nil
  })

  if err != nil {
    log.Printf("Error while cleaning up empty dirs: %v", err)
  }
  
  removeEmptyDirs(dirs)
}

func removeEmptyDirs(dirs []string) {
  sort.Sort(ByLength(dirs))

  for _, dirpath := range dirs {
    entries, err := ioutil.ReadDir(dirpath)
    if err != nil { continue }

    if len(entries) == 0 {
      log.Printf("Removing empty dir %v", dirpath)

      err = os.Remove(dirpath)
      if err != nil {
        log.Printf("Error while removing dir %v: %v", dirpath, err)
      }
    }
  }
}

func (pr *ProgressReporter) accountRemove(progress int64) {
  pr.progressWG.Add(1)
  go func() {
    pr.progressChan <- (progress*RemoveFactor)/100
  }()
}

func (pr *ProgressReporter) accountUpdate(progress int64) {
  pr.progressWG.Add(1)
  go func() {
    pr.progressChan <- (progress*UpdateFactor)/100
  }()
}

func (pr *ProgressReporter) accountAdd(progress int64) {
  pr.progressWG.Add(1)
  go func() {
    pr.progressChan <- (progress*AddFactor)/100
  }()
}

func (pr *ProgressReporter) accountBackupRemove() {
  // exact size of files is not known when removeBackups()
  // so using some arbitrary value (fair dice roll)
  pr.progressWG.Add(1)
  go func() {
    pr.progressChan <- RemoveBackupPrice
  }()
}

func (pr *ProgressReporter) reportingLoop() {
  for chunk := range pr.progressChan {
    pr.currentProgress += uint64(chunk)

    percent := (pr.currentProgress*100) / pr.grandTotal

    percentsChanged := int(percent) > pr.percent
    pr.percent = int(percent)

    if percentsChanged {
      pr.progressHandler.HandlePercentChange(pr.percent)
    }
    
    pr.progressWG.Done()
  }
  
  log.Println("Reporting loop finished")
}

func (pr *ProgressReporter) waitProgressReported() {
  log.Println("Waiting for progress reporting to finish")
  pr.progressWG.Wait()
}

func (pr *ProgressReporter) shutdown() {
  log.Println("Shutting down progress reporter...")
  close(pr.progressChan)
  go func() {
    pr.finished <- true
  }()
}

func (pr *ProgressReporter) sendSystemMessage(msg string) {
  pr.systemMessageChan <- msg
}

func (pr *ProgressReporter) receiveSystemMessages() {
  for msg := range pr.systemMessageChan {
    pr.progressHandler.HandleSystemMessage(msg)
  }
  
  log.Println("System messages handling finished")
}

func (pr *ProgressReporter) receiveFinish() {
  log.Println("Waiting for teardown and global finish...")
  <- pr.finished
  pr.progressHandler.HandleFinish()
}

func (pr *ProgressReporter) handleProgress() {
  go pr.receiveSystemMessages()
}

func (ph *LogProgressHandler) HandlePercentChange(percent int) {
  log.Printf("Completed %v%%", percent)
}

func (ph *LogProgressHandler) HandleSystemMessage(msg string) {
  log.Printf("System message: %v", msg)
}

func (ph *LogProgressHandler) HandleFinish() {
  log.Println("Finished")
}
