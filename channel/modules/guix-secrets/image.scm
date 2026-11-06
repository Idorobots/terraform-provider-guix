;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix-secrets image)
  #:use-module ((gnu packages base) #:select (coreutils))
  #:use-module (gnu packages linux)
  #:use-module (guix-secrets files)
  #:use-module (guix-secrets layout)
  #:use-module (guix-secrets requests)
  #:use-module (gnu image)
  #:use-module (gnu system uuid)
  #:use-module (guix-secrets tools)
  #:autoload (guix-secrets helper) (image-helper)
  #:autoload (guix-secrets privileged) (provision-with-mount)
  #:use-module (guix build utils)
  #:use-module ((guix build syscalls) #:select (mount-points))
  #:use-module (json)
  #:use-module (ice-9 ftw)
  #:use-module (ice-9 popen)
  #:use-module (ice-9 textual-ports)
  #:use-module (srfi srfi-1)
  #:use-module (ice-9 match)
  #:export (image-extents provision-image call-with-fuse-mount provision-plan))

(define (check-file-system image declared offset size)
  (when (supported-file-system? (partition-file-system declared))
    (let* ((output (command-output
                    (tool util-linux "sbin/blkid") "-p" "-O" (number->string offset)
                    "-S" (number->string size) "-o" "export" image))
           (fields (map (lambda (line)
                          (let ((at (string-index line #\=)))
                            (if at (cons (substring line 0 at) (substring line (+ at 1)))
                                (cons "" ""))))
                        (string-split output #\newline)))
           (uuid (partition-uuid declared)))
      (unless (and (equal? (assoc-ref fields "TYPE") (partition-file-system declared))
                   (equal? (or (assoc-ref fields "LABEL") "") (partition-label declared))
                   ;; Guix assigns root a fresh deterministic UUID during the
                   ;; build, replacing partition-uuid from the input record.
                   (or (not uuid) (memq 'boot (partition-flags declared))
                       (equal? (assoc-ref fields "UUID") (uuid->string uuid))))
        (throw 'secrets-error "artifact filesystem identity does not match declaration")))))

(define (image-extents image declaration)
  "Read actual geometry; verify filesystem types and labels against DECLARATION."
  (let* ((table (assoc-ref (json-string->scm
                           (command-output (tool util-linux "sbin/sfdisk") "--json" image))
                          "partitiontable"))
         (sector-size (assoc-ref table "sectorsize"))
         (partitions (assoc-ref table "partitions"))
         (image-size (stat:size (stat image))))
    (unless (and (member (assoc-ref table "label") '("dos" "gpt"))
                 (equal? (assoc-ref table "unit") "sectors")
                 (member sector-size '(512 4096)) (vector? partitions))
      (throw 'secrets-error "expected a raw MBR or GPT disk image"))
    (unless (and (= (vector-length partitions) (length (image-partitions declaration)))
                 (equal? (assoc-ref table "label")
                         (if (eq? 'gpt (image-partition-table-type declaration)) "gpt" "dos")))
      (throw 'secrets-error "artifact partition table does not match declaration"))
    (map
     (lambda (partition declared)
       (let ((start (assoc-ref partition "start"))
             (count (assoc-ref partition "size")))
         (unless (and (integer? start) (>= start 0)
                      (integer? count) (> count 0)
                      (<= (* (+ start count) sector-size) image-size))
           (throw 'secrets-error "invalid image partition bounds"))
         (let ((offset (* start sector-size)) (size (* count sector-size)))
           (check-file-system image declared offset size)
           (list offset size))))
     (vector->list partitions) (image-partitions declaration))))

(define (capability-error? log)
  (let ((text (string-downcase (call-with-input-file log get-string-all))))
    (or (any (lambda (message) (string-contains text message))
             '("fuse: device not found" "failed to open /dev/fuse"
               "fuse: failed to exec fusermount"))
        ;; libfuse/fusermount releases vary in their wording. Require a mount
        ;; helper diagnostic plus a capability/permission error, not an ext4
        ;; access or consistency failure from fuse2fs itself.
        (and (or (string-contains text "fuse:")
                 (string-contains text "fusermount3:") (string-contains text "fusermount:"))
             (string-contains text "mount")
             (any (lambda (message) (string-contains text message))
                  '("operation not permitted" "permission denied"
                    "operation not supported" "function not implemented"))))))

(define (call-with-fuse-mount image offset directory log proc)
  "Call PROC with a private mounted root, or return #f for unavailable FUSE."
  (if (not (and (access? "/dev/fuse" R_OK) (access? "/dev/fuse" W_OK)
                (or (which "fusermount3") (which "fusermount"))))
      #f
      (let* ((fuse2fs (tool e2fsprogs "bin/fuse2fs"))
             (unmount (or (which "fusermount3") (which "fusermount")))
             (mounted? #f) (reaped? #f)
             (pid (spawn-logged fuse2fs log "-f" "-o"
                                (format #f "rw,fakeroot,nodev,nosuid,noexec,offset=~a" offset)
                                image directory)))
        (dynamic-wind
          (const #t)
          (lambda ()
            (let loop ((attempt 0))
              (let ((status (waitpid pid WNOHANG)))
                (cond
                 ((not (zero? (car status)))
                  (set! reaped? #t)
                  (if (capability-error? log) #f
                      (throw 'secrets-error "FUSE image mount failed; image was not provisioned")))
                 ((member directory (mount-points))
                  (set! mounted? #t)
                  (unless unmount (error "fusermount is required to release the image"))
                  (proc directory)
                  #t)
                 ((>= attempt 100) (error "FUSE image mount timed out"))
                 (else (usleep 100000) (loop (+ attempt 1)))))))
          (lambda ()
            (when (member directory (mount-points))
              (unless (and unmount (zero? (system* unmount "-u" "--" directory)))
                (throw 'mount-cleanup-failed directory)))
            (unless reaped?
              ;; An unmounted FUSE foreground process must finish flushing before
              ;; we publish its backing image. On startup failure stop it first.
              (unless mounted? (false-if-exception (kill pid SIGTERM)))
              (let ((status (cdr (waitpid pid))))
                (when (and mounted? (not (zero? status)))
                  (throw 'mount-cleanup-failed directory)))))))))

(define (polkit-provision image plan)
  (if (zero? (getuid))
      (provision-with-mount image plan (tool util-linux "bin/mount")
                            (tool util-linux "bin/umount"))
      (let ((pkexec (which "pkexec")))
        (unless pkexec
          (throw 'secrets-error "kernel mounting requires pkexec and a Polkit authentication agent"))
        (display "guix secrets: requesting Polkit authentication for kernel mounting\n"
                 (current-error-port))
        (let ((pipe (open-pipe* OPEN_WRITE pkexec (image-helper))))
          (set-port-encoding! pipe "UTF-8")
          (write (list image plan) pipe)
          (newline pipe)
          (let ((status (close-pipe pipe)))
            (when (equal? 2 (status:exit-val status))
              (throw 'mount-cleanup-failed image))
            (unless (zero? status)
              (throw 'secrets-error "Polkit image provisioning failed or authentication was cancelled")))))))

(define (provision-plan copy mountpoint log plan)
  (let loop ((remaining plan))
    (unless (null? remaining)
      (match (car remaining)
        ((offset size type options files)
         (if (and (string-prefix? "ext" type) (null? options)
                  (call-with-fuse-mount copy offset mountpoint log
                                        (lambda (root) (install-payload root files))))
             (loop (cdr remaining))
             ;; One authentication for all remaining mounts, not one per file.
             (polkit-provision copy remaining)))))))

(define* (provision-image input output files declaration
                          #:key (evaluate (lambda (thunk) (thunk))))
  (validate-payload files)
  (let* ((input (canonicalize-path input))
         (parent (canonicalize-path (dirname output)))
         (output (string-append parent "/" (basename output))))
    (when (or (string-prefix? "/gnu/store/" output)
              (false-if-exception (lstat output)))
      (throw 'secrets-error "output must be a new path outside /gnu/store"))
    (unless (eq? 'regular (stat:type (stat input)))
      (error "input must be a regular disk image"))
    ;; Validate the image before attempting any authentication or writable mount.
    (let* ((extents (image-extents input declaration))
           (plan (validate-plan
                  (evaluate (lambda () (provisioning-plan declaration extents files)))
                  (stat:size (stat input))))
           (directory (mkdtemp (string-append parent "/.guix-secrets-XXXXXX")))
           (copy (string-append directory "/image"))
           (mountpoint (string-append directory "/root"))
           (log (string-append directory "/fuse.log"))
           (safe-to-clean? #t))
      (chmod directory #o700)
      (dynamic-wind
        (const #t)
        (lambda ()
          (checked-command (tool coreutils "bin/cp") "--reflink=auto" "--sparse=always"
                           "--" input copy)
          (chmod copy #o600)
          (mkdir mountpoint #o700)
          (catch 'mount-cleanup-failed
            (lambda ()
              (provision-plan copy mountpoint log plan))
            (lambda args
              (set! safe-to-clean? #f)
              (display "guix secrets: unmount failed; retaining private workspace: "
                       (current-error-port))
              (display directory (current-error-port)) (newline (current-error-port))
              (apply throw args)))
          ;; link is atomic and refuses to overwrite an existing output.
          (link copy output))
        (lambda ()
          (when safe-to-clean?
            (when (file-exists? copy) (delete-file copy))
            (when (file-exists? log) (delete-file log))
            (when (file-exists? mountpoint) (rmdir mountpoint))
            (rmdir directory)))))))
