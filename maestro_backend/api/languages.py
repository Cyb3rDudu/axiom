"""
Language Management API Endpoints

Provides endpoints for:
- Listing supported languages
- Getting language details
- Managing language preferences
"""

from fastapi import APIRouter, Depends, HTTPException, status
from sqlalchemy.orm import Session
from typing import List

from database.database import get_db
from database.models import SupportedLanguage
from api import schemas

router = APIRouter(prefix="/api/languages", tags=["languages"])


@router.get("/", response_model=List[schemas.SupportedLanguage])
async def list_languages(
    db: Session = Depends(get_db),
    include_inactive: bool = False
):
    """
    List all supported languages.

    Args:
        include_inactive: If True, include inactive languages. Default: False

    Returns:
        List of supported languages with their metadata
    """
    query = db.query(SupportedLanguage)

    if not include_inactive:
        query = query.filter(SupportedLanguage.is_active == True)

    languages = query.order_by(
        SupportedLanguage.completion_percentage.desc(),
        SupportedLanguage.code
    ).all()

    return languages


@router.get("/{code}", response_model=schemas.SupportedLanguage)
async def get_language(
    code: str,
    db: Session = Depends(get_db)
):
    """
    Get specific language details by language code.

    Args:
        code: Language code (e.g., 'en', 'de', 'fr')

    Returns:
        Language details

    Raises:
        404: Language not found
    """
    language = db.query(SupportedLanguage).filter(
        SupportedLanguage.code == code
    ).first()

    if not language:
        raise HTTPException(
            status_code=status.HTTP_404_NOT_FOUND,
            detail=f"Language '{code}' not found"
        )

    return language
