;;; SPDX-License-Identifier: MPL-2.0
;;; Local file installation for mounted images.
(define-module (guix-secrets files)
  #:use-module (ice-9 match)
  #:use-module (ice-9 ftw)
  #:use-module (rnrs bytevectors)
  #:use-module (rnrs io ports)
  #:use-module (srfi srfi-1)
  #:export (valid-destination? validate-payload install-payload read-bytes))

(define (valid-destination? path)
  (and (string? path) (string-prefix? "/" path) (not (string=? path "/"))
       (not (string-prefix? "/gnu/store/" path))
       (not (string=? path "/gnu/store"))
       (every (lambda (part) (not (member part '("" "." ".."))))
              (cdr (string-split path #\/)))
       (not (string-index path #\nul))))

(define (read-bytes file)
  (call-with-input-file file
    (lambda (port)
      (let ((bytes (get-bytevector-all port)))
        (if (eof-object? bytes) (make-bytevector 0) bytes)))
    #:binary #t))

(define (validate-payload files)
  ;; Never include payload values in exceptions or diagnostics.
  (unless (list? files) (error "invalid secret file list"))
  (let ((names '()))
    (for-each
     (lambda (file)
       (match file
         ((destination mode (? bytevector?))
          (unless (and (valid-destination? destination)
                       (integer? mode) (exact? mode) (<= 0 mode #o777))
            (error "invalid secret destination or mode"))
          (when (member destination names)
            (error "duplicate secret destination"))
          (set! names (cons destination names)))
         (_ (error "invalid secret file entry"))))
     files))
  files)

(define (maybe-lstat path)
  (catch 'system-error (lambda () (lstat path))
    (lambda args
      (if (= ENOENT (system-error-errno args)) #f (apply throw args)))))

(define* (install-payload root files #:key (owner 0) (group 0))
  "Install validated FILES under ROOT. No target code is executed."
  (validate-payload files)
  (define (open-directory path)
    (open-fdes path (logior O_RDONLY O_DIRECTORY O_NOFOLLOW O_CLOEXEC)))
  (let ((root-fd (open-directory (canonicalize-path root))))
    ;; Anchor each component to an open directory descriptor. Reject symlinks
    ;; even if a parent changes between preflight and installation. /proc is a
    ;; runner/host facility; no /proc mount is needed inside the offline image.
    (define (with-parent destination create? proc)
      (let loop ((parts (cdr (string-split destination #\/))) (fd root-fd))
        (let ((prefix (format #f "/proc/self/fd/~a/" fd)))
          (if (null? (cdr parts))
              (proc (string-append prefix (car parts)))
              (let* ((path (string-append prefix (car parts)))
                     (info (maybe-lstat path)))
                (when (or info create?)
                  (unless info (mkdir path #o700))
                  (let ((child (open-directory path)))
                    (dynamic-wind
                      (const #t)
                      (lambda ()
                        (unless info (chown child owner group))
                        (loop (cdr parts) child))
                      (lambda () (close-fdes child))))))))))
    (dynamic-wind
      (const #t)
      (lambda ()
        ;; Preflight every existing parent/leaf before changing any file.
        (for-each
         (match-lambda
           ((destination _ _)
            (with-parent destination #f
              (lambda (path)
                (let ((info (maybe-lstat path)))
                  (when (and info (not (eq? 'regular (stat:type info))))
                    (error "secret destination must be a regular file")))))))
         files)
        (for-each
         (match-lambda
           ((destination mode bytes)
            (with-parent destination #t
              (lambda (path)
                (let* ((template (string-append path ".guix-secrets-XXXXXX"))
                       (port (mkstemp! template)))
                  (dynamic-wind
                    (const #t)
                    (lambda ()
                      (chmod port #o600)
                      (put-bytevector port bytes)
                      (force-output port)
                      (chown port owner group)
                      (chmod port mode)
                      (fsync port)
                      (close-port port)
                      (rename-file template path))
                    (lambda ()
                      (unless (port-closed? port) (close-port port))
                      (when (file-exists? template) (delete-file template)))))))))
         files))
      (lambda () (close-fdes root-fd)))))
