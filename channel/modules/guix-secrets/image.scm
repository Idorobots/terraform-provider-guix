;;; SPDX-License-Identifier: MPL-2.0
(define-module (guix-secrets image)
  #:use-module ((gnu packages base) #:select (coreutils))
  #:use-module (gnu packages linux)
  #:use-module (guix-secrets files)
  #:use-module (guix-secrets tools)
  #:autoload (guix-secrets helper) (image-helper)
  #:use-module (guix build utils)
  #:use-module ((guix build syscalls) #:select (mount-points))
  #:use-module (json)
  #:use-module (ice-9 ftw)
  #:use-module (ice-9 popen)
  #:use-module (ice-9 textual-ports)
  #:use-module (rnrs bytevectors)
  #:use-module ((rnrs io ports) #:select (get-bytevector-n))
  #:use-module (srfi srfi-1)
  #:export (image-partition provision-image call-with-fuse-mount))

(define (partition-ext4-label image offset size)
  (and (>= size 2048)
       (call-with-input-file image
         (lambda (port)
           (seek port (+ offset 1024) SEEK_SET)
           (let ((super (get-bytevector-n port 136)))
             (and (bytevector? super) (= 136 (bytevector-length super))
                  (= #xef53 (bytevector-u16-ref super 56 (endianness little)))
                  (let loop ((i 120) (chars '()))
                    (if (or (= i 136) (zero? (bytevector-u8-ref super i)))
                        (list->string (reverse chars))
                        (loop (+ i 1) (cons (integer->char (bytevector-u8-ref super i)) chars)))))))
         #:binary #t)))

(define* (image-partition image #:optional root-label)
  "Return (OFFSET SIZE) for an unambiguous ext4 partition of a raw image."
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
    (let ((matches
           (filter-map
            (lambda (partition)
              (let ((start (assoc-ref partition "start"))
                    (count (assoc-ref partition "size")))
                (unless (and (integer? start) (>= start 0)
                             (integer? count) (> count 0)
                             (<= (* (+ start count) sector-size) image-size))
                  (error "invalid image partition bounds"))
                (let* ((offset (* start sector-size)) (size (* count sector-size))
                       (label (partition-ext4-label image offset size)))
                  (and label (or (not root-label) (string=? root-label label))
                       (list offset size)))))
            (vector->list partitions))))
      (unless (= 1 (length matches))
        (throw 'secrets-error "expected one ext4 root partition; use --root-label to select it"))
      (car matches))))

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

(define (polkit-provision image partition files)
  (let ((pkexec (which "pkexec")))
    (unless pkexec
      (display "guix secrets: FUSE unavailable; install pkexec and a Polkit authentication agent\n"
               (current-error-port))
      (throw 'secrets-error "Polkit is unavailable"))
    (display "guix secrets: FUSE unavailable; requesting Polkit authentication\n"
             (current-error-port))
    (let ((pipe (open-pipe* OPEN_WRITE pkexec (image-helper))))
      (set-port-encoding! pipe "UTF-8")
      (write (append (list image) partition (list files)) pipe)
      (newline pipe)
      (let ((status (close-pipe pipe)))
        (when (equal? 2 (status:exit-val status))
          (throw 'mount-cleanup-failed image))
        (unless (zero? status)
          (throw 'secrets-error "Polkit image provisioning failed or authentication was cancelled"))))))

(define* (provision-image input output files #:optional root-label)
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
    (let* ((partition (image-partition input root-label))
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
              (unless (call-with-fuse-mount
                       copy (car partition) mountpoint log
                       (lambda (root) (install-payload root files)))
                (polkit-provision copy partition files)))
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
