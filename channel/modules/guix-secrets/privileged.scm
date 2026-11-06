;;; SPDX-License-Identifier: MPL-2.0
;;; Immutable Polkit entry point. Requests contain data, never executable Scheme.
(define-module (guix-secrets privileged)
  #:use-module (guix-secrets files)
  #:use-module (guix-secrets requests)
  #:use-module (ice-9 ftw)
  #:use-module (ice-9 match)
  #:export (provision-with-mount mount-options))

(define (mount-options offset size options)
  (string-join
   (append (list (format #f "loop,offset=~a,sizelimit=~a,nodev,nosuid,noexec" offset size))
           options) ","))

(define (provision-with-mount image plan mount umount)
  (unless (and (= 0 (getuid)) (string-prefix? "/" image)
               (not (string-prefix? "/gnu/store/" (canonicalize-path image)))
               (eq? 'regular (stat:type (lstat image))))
    (error "invalid private image"))
  (validate-plan plan (stat:size (stat image)))
  (for-each
   (match-lambda
     ((offset size type options files)
      (let ((directory (mkdtemp "/tmp/guix-secrets-mount-XXXXXX")) (mounted? #f))
        (chmod directory #o700)
        (dynamic-wind
          (const #t)
          (lambda ()
            (unless (zero? (system* mount "-t" type "-o" (mount-options offset size options)
                                   "--" image directory))
              (error "privileged image mount failed"))
            (set! mounted? #t)
            (install-payload directory files))
          (lambda ()
            (when (and mounted? (not (zero? (system* umount "--" directory))))
              (format (current-error-port) "guix secrets: unmount failed; retained at ~a~%" directory)
              (throw 'mount-cleanup-failed directory))
            (rmdir directory))))))
   plan))
