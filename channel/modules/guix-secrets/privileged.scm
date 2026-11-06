;;; SPDX-License-Identifier: MPL-2.0
;;; The Polkit entry point is built into the immutable store, never extracted
;;; executable code in a user-writable directory. Stdin is data, not Scheme code.
(define-module (guix-secrets privileged)
  #:use-module (guix-secrets files)
  #:use-module (ice-9 ftw)
  #:use-module (ice-9 match)
  #:export (provision-with-mount))

(define (checked program . args)
  (unless (zero? (apply system* program args))
    (error "privileged image tool failed")))

(define (provision-with-mount image offset size files mount umount)
  (validate-payload files)
  (unless (and (= 0 (getuid))
               (string-prefix? "/" image)
               (not (string-prefix? "/gnu/store/" (canonicalize-path image)))
               (eq? 'regular (stat:type (lstat image)))
               (integer? offset) (>= offset 0)
               (integer? size) (> size 0)
               (<= (+ offset size) (stat:size (stat image))))
    (error "invalid private image or partition bounds"))
  (let ((directory (mkdtemp "/tmp/guix-secrets-mount-XXXXXX"))
        (mounted? #f))
    (chmod directory #o700)
    (dynamic-wind
      (const #t)
      (lambda ()
        (checked mount "-t" "ext4" "-o"
                 (format #f "loop,offset=~a,sizelimit=~a,nodev,nosuid,noexec" offset size)
                 "--" image directory)
        (set! mounted? #t)
        (install-payload directory files))
      (lambda ()
        ;; A normal umount flushes writes and releases mount's autoclear loop.
        ;; Never remove the mount directory when unmount failed.
        (when (and mounted? (not (zero? (system* umount "--" directory))))
          (format (current-error-port)
                  "guix secrets: unmount failed; private mount retained at ~a~%" directory)
          (throw 'mount-cleanup-failed directory))
        (rmdir directory)))))
