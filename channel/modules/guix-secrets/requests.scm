;;; SPDX-License-Identifier: MPL-2.0
;;; Guile-only validation shared with the immutable privileged helper.
(define-module (guix-secrets requests)
  #:use-module (guix-secrets files)
  #:use-module (ice-9 match)
  #:use-module (srfi srfi-1)
  #:export (supported-file-system? validate-plan))

(define (supported-file-system? type)
  (member type '("ext2" "ext3" "ext4" "btrfs")))

(define (validate-plan plan image-size)
  (unless (list? plan) (error "invalid mount plan"))
  (for-each
   (match-lambda
     ((offset size type options files)
      (unless (and (integer? offset) (>= offset 0) (integer? size) (> size 0)
                   (<= (+ offset size) image-size) (supported-file-system? type)
                   (list? options)
                   (every (lambda (s)
                            (and (string? s) (not (string-index s #\,))
                                 (not (string-index s #\nul))
                                 (or (string-prefix? "subvol=" s)
                                     (string-prefix? "subvolid=" s)))) options)
                   (or (string=? type "btrfs") (null? options)))
        (error "invalid filesystem mount request"))
      (validate-payload files))
     (_ (error "invalid mount request")))
   plan)
  plan)
